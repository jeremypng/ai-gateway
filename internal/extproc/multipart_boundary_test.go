// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/endpointspec"
	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/translator"
)

func TestUpstreamMultipartUsesFilteredBoundaryForFirstAttemptAndRetry(t *testing.T) {
	original, originalType := buildTestMultipartBody(t, map[string]string{"model": "whisper-1", "prompt": "original-private-value"}, "fixture.wav", []byte("fixture-audio"))
	filtered, filteredType := buildTestMultipartBody(t, map[string]string{"model": "whisper-1", "prompt": "[[EMAIL_1]]"}, "fixture.wav", []byte("fixture-audio"))
	require.NotEqual(t, originalType, filteredType)
	spec := endpointspec.TranscriptionEndpointSpec{}
	_, parsed, _, _, err := spec.ParseMultipartBody(original, originalType, false)
	require.NoError(t, err)
	parent := &transcriptionProcessorRouterFilter{
		originalRequestBodyRaw: original, originalRequestBody: parsed,
		requestHeaders: map[string]string{"content-type": originalType},
		config:         &filterapi.RuntimeConfig{}, upstreamFilterCount: 1,
	}
	input, contentType := filtered, filteredType
	for attempt := 1; attempt <= 2; attempt++ {
		parent.upstreamFilterCount = attempt
		tr := translator.NewTranscriptionOpenAIToOpenAITranslator("", "provider-model")
		tr.(translator.ContentTypeSetter).SetContentType(originalType)
		processor := &transcriptionProcessorUpstreamFilter{
			parent: parent, translator: tr, metrics: &mockMetrics{}, logger: slog.Default(),
			requestHeaders: map[string]string{":path": "/v1/audio/transcriptions", "content-type": contentType},
		}
		_, err = processor.ProcessRequestHeaders(t.Context(), &corev3.HeaderMap{Headers: []*corev3.HeaderValue{{Key: "content-type", RawValue: []byte(contentType)}}})
		require.NoError(t, err)
		response, err := processor.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: input, EndOfStream: true})
		require.NoError(t, err)
		require.Nil(t, response.GetImmediateResponse())
		common := response.GetRequestBody().GetResponse()
		output := common.GetBodyMutation().GetBody()
		var outputType string
		for _, h := range common.GetHeaderMutation().GetSetHeaders() {
			if h.GetHeader().GetKey() == "content-type" {
				outputType = string(h.GetHeader().GetRawValue())
			}
		}
		_, params, err := mime.ParseMediaType(outputType)
		require.NoError(t, err)
		reader := multipart.NewReader(bytes.NewReader(output), params["boundary"])
		fields := map[string]string{}
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			data, err := io.ReadAll(part)
			require.NoError(t, err)
			fields[part.FormName()] = string(data)
		}
		require.Equal(t, "[[EMAIL_1]]", fields["prompt"])
		require.Equal(t, "fixture-audio", fields["file"])
		require.Equal(t, "provider-model", fields["model"])
		input, contentType = output, outputType
	}
}
