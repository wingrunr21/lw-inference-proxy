package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		wantModel string
		wantErr   error
	}{
		{
			name:      "model at start of object",
			body:      `{"model":"gpt-4","messages":[]}`,
			wantModel: "gpt-4",
		},
		{
			name:      "model buried after other fields",
			body:      `{"stream":true,"temperature":0.7,"model":"claude-3","messages":[{"role":"user","content":"hi"}]}`,
			wantModel: "claude-3",
		},
		{
			name:    "missing model field",
			body:    `{"messages":[{"role":"user","content":"hi"}]}`,
			wantErr: errMissingModel,
		},
		{
			name:    "model field with non-string value (number)",
			body:    `{"model":42}`,
			wantErr: errInvalidModelField,
		},
		{
			name:    "model field with non-string value (null)",
			body:    `{"model":null}`,
			wantErr: errInvalidModelField,
		},
		{
			name:    "model field with empty string",
			body:    `{"model":""}`,
			wantErr: errInvalidModelField,
		},
		{
			name:    "malformed JSON",
			body:    `{not valid json`,
			wantErr: errInvalidJSON,
		},
		{
			name:    "body is JSON array not object",
			body:    `["model","value"]`,
			wantErr: errNotObject,
		},
		{
			name:    "empty body",
			body:    ``,
			wantErr: errInvalidJSON,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := io.NopCloser(strings.NewReader(tt.body))

			model, restored, err := extractModel(body)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.Empty(t, model)
				return
			}

			must := require.New(t)
			must.NoError(err)
			assert.Equal(t, tt.wantModel, model)

			// Verify the restored body contains the full original payload.
			must.NotNil(restored)
			restoredBytes, readErr := io.ReadAll(restored)
			must.NoError(readErr)
			assert.JSONEq(t, tt.body, string(restoredBytes))
		})
	}
}

func TestExtractModel_NilBody(t *testing.T) {
	t.Parallel()
	_, _, err := extractModel(nil)
	assert.ErrorIs(t, err, errMissingModel)
}

func TestExtractModel_NoBody(t *testing.T) {
	t.Parallel()
	_, _, err := extractModel(http.NoBody)
	assert.ErrorIs(t, err, errMissingModel)
}

func TestExtractModel_RestoredBodyIsFullPayload(t *testing.T) {
	t.Parallel()
	// Payload with model early and large content after.
	payload := `{"model":"gpt-4","messages":[{"role":"user","content":"` +
		strings.Repeat("x", 4096) + `"}]}`

	model, restored, err := extractModel(io.NopCloser(strings.NewReader(payload)))

	must := require.New(t)
	must.NoError(err)
	assert.Equal(t, "gpt-4", model)

	got, err := io.ReadAll(restored)
	must.NoError(err)
	assert.JSONEq(t, payload, string(got))
}
