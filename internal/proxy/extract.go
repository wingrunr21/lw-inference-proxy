package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// extractModel reads the minimum necessary bytes from body to find the top-level
// "model" field, then reconstructs the complete original body as an io.ReadCloser
// for forwarding verbatim to the backend.
//
// Only the small prefix consumed by the JSON tokenizer is held in memory.
// The remainder of the body — including large prompts or multimodal payloads —
// is piped directly to the backend without buffering.
func extractModel(body io.ReadCloser) (model string, restored io.ReadCloser, err error) {
	if body == nil || body == http.NoBody {
		return "", http.NoBody, errMissingModel
	}

	var buf bytes.Buffer
	// TeeReader copies every byte read by the decoder into buf.
	// After extraction, buf holds the full decoder read-ahead and body holds the rest.
	tee := io.TeeReader(body, &buf)
	dec := json.NewDecoder(tee)

	tok, err := dec.Token()
	if err != nil {
		return "", nil, errInvalidJSON
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", nil, errNotObject
	}

	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return "", nil, errInvalidJSON
		}
		key, ok := tok.(string)
		if !ok {
			return "", nil, errInvalidJSON
		}

		if key == "model" {
			tok, err = dec.Token()
			if err != nil {
				return "", nil, errInvalidJSON
			}
			m, ok := tok.(string)
			if !ok || m == "" {
				return "", nil, errInvalidModelField
			}
			// buf contains all bytes physically read by the decoder (including read-ahead).
			// body still holds all bytes the decoder has NOT yet read.
			// MultiReader reconstructs the complete original body stream.
			restored = io.NopCloser(io.MultiReader(bytes.NewReader(buf.Bytes()), body))
			return m, restored, nil
		}

		// Skip the value for this key.
		var skip json.RawMessage
		if err = dec.Decode(&skip); err != nil {
			return "", nil, errInvalidJSON
		}
	}

	return "", nil, errMissingModel
}

var (
	errInvalidJSON       = errors.New("invalid JSON body")
	errNotObject         = errors.New("request body must be a JSON object")
	errInvalidModelField = errors.New(`"model" must be a non-empty string`)
	errMissingModel      = errors.New(`missing required field "model"`)
)
