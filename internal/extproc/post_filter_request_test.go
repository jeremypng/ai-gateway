// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/json"
	"github.com/envoyproxy/ai-gateway/internal/translator"
)

type postFilterAuthRecorder struct{ bodies [][]byte }

func (r *postFilterAuthRecorder) Do(_ context.Context, _ map[string]string, body []byte) ([]internalapi.Header, error) {
	r.bodies = append(r.bodies, bytes.Clone(body))
	return nil, nil
}

func TestUpstreamTranslationPreservesPostFilterContent(t *testing.T) {
	for _, tc := range []struct {
		name          string
		newTranslator func() translator.OpenAIChatCompletionTranslator
	}{
		{name: "OpenAI model override", newTranslator: func() translator.OpenAIChatCompletionTranslator {
			return translator.NewChatCompletionOpenAIToOpenAITranslator("", "provider-model")
		}},
		{name: "AWS Bedrock Converse", newTranslator: func() translator.OpenAIChatCompletionTranslator {
			return translator.NewChatCompletionOpenAIToAWSBedrockTranslator("anthropic.claude-sonnet-4-5")
		}},
		{name: "AWS Anthropic", newTranslator: func() translator.OpenAIChatCompletionTranslator {
			return translator.NewChatCompletionOpenAIToAWSAnthropicTranslator("bedrock-2023-05-31", "anthropic.claude-sonnet-4-5")
		}},
		{name: "GCP Anthropic", newTranslator: func() translator.OpenAIChatCompletionTranslator {
			return translator.NewChatCompletionOpenAIToGCPAnthropicTranslator("vertex-2023-10-16", "claude-sonnet-4-5")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, streamed := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t", streamed), func(t *testing.T) {
					original := []byte(fmt.Sprintf(`{"model":"alias","messages":[{"role":"user","content":"original-private-value"}],"max_tokens":100,"stream":%t}`, streamed))
					filtered := []byte(fmt.Sprintf(`{"model":"alias","messages":[{"role":"user","content":"[[EMAIL_1]]"}],"max_tokens":100,"stream":%t}`, streamed))
					var parsed openai.ChatCompletionRequest
					require.NoError(t, json.Unmarshal(original, &parsed))
					parent := &chatCompletionProcessorRouterFilter{
						originalRequestBodyRaw: original,
						originalRequestBody:    &parsed,
						config:                 &filterapi.RuntimeConfig{},
						upstreamFilterCount:    1,
					}
					processor := &chatCompletionProcessorUpstreamFilter{
						parent:            parent,
						translator:        tc.newTranslator(),
						metrics:           &mockMetrics{},
						logger:            slog.Default(),
						requestHeaders:    map[string]string{":path": "/v1/chat/completions"},
						modelNameOverride: "provider-model",
					}
					auth := &postFilterAuthRecorder{}
					processor.handler = auth
					_, err := processor.ProcessRequestHeaders(t.Context(), nil)
					require.NoError(t, err)
					require.Empty(t, auth.bodies, "payload auth waits for the filtered request body")
					response, err := processor.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: filtered, EndOfStream: true})
					require.NoError(t, err)
					body := response.GetRequestBody().GetResponse().GetBodyMutation().GetBody()
					require.Contains(t, string(body), "[[EMAIL_1]]")
					require.NotContains(t, string(body), "original-private-value")
					require.Equal(t, body, auth.bodies[0], "payload auth receives the final translated body")
					parent.upstreamFilterCount = 2
					retry := *processor
					retry.translator = tc.newTranslator()
					_, err = retry.ProcessRequestHeaders(t.Context(), nil)
					require.NoError(t, err)
					response, err = retry.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: body, EndOfStream: true})
					require.NoError(t, err)
					retriedBody := response.GetRequestBody().GetResponse().GetBodyMutation().GetBody()
					require.Contains(t, string(retriedBody), "[[EMAIL_1]]")
					require.NotContains(t, string(retriedBody), "original-private-value")
					require.Equal(t, retriedBody, auth.bodies[1])
					require.JSONEq(t, string(filtered), string(parent.filteredRequestBodyRaw))
				})
			}
		})
	}
}

func TestUpstreamTranslationRejectsInvalidPostFilterBody(t *testing.T) {
	original := []byte(`{"model":"alias","messages":[{"role":"user","content":"original-private-value"}]}`)
	var parsed openai.ChatCompletionRequest
	require.NoError(t, json.Unmarshal(original, &parsed))
	auth := &postFilterAuthRecorder{}
	processor := &chatCompletionProcessorUpstreamFilter{
		parent:         &chatCompletionProcessorRouterFilter{originalRequestBodyRaw: original, originalRequestBody: &parsed, config: &filterapi.RuntimeConfig{}},
		translator:     translator.NewChatCompletionOpenAIToOpenAITranslator("", "provider-model"),
		metrics:        &mockMetrics{},
		logger:         slog.Default(),
		requestHeaders: map[string]string{":path": "/v1/chat/completions"},
		handler:        auth,
	}
	response, err := processor.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: []byte("invalid JSON"), EndOfStream: true})
	require.NoError(t, err)
	require.NotNil(t, response.GetImmediateResponse())
	require.Equal(t, 400, int(response.GetImmediateResponse().GetStatus().GetCode()))
	require.Empty(t, auth.bodies)
}
