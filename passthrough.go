package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (b *bridge) unknownEndpoint(w http.ResponseWriter, r *http.Request) {
	c, err := loadConfig(b.path)
	if err != nil {
		apiError(w, 503, "bridge config is invalid; run sync to see the validation error")
		return
	}
	// shortcut: replay buffers up to 32 MiB; spool to disk when larger uploads are needed.
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		apiError(w, 413, "request exceeds 32 MiB or could not be read")
		return
	}
	model := r.URL.Query().Get("model")
	if encoding := r.Header.Get("Content-Encoding"); encoding == "" || encoding == "identity" {
		mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType == "multipart/form-data" {
			parts := multipart.NewReader(bytes.NewReader(data), params["boundary"])
			for {
				part, err := parts.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					apiError(w, 400, "invalid multipart body")
					return
				}
				if part.FormName() == "model" && part.FileName() == "" {
					value, err := io.ReadAll(part)
					if err != nil {
						apiError(w, 400, "invalid multipart model")
						return
					}
					model = string(value)
				}
			}
		} else {
			var body struct{ Model string }
			if json.Unmarshal(data, &body) == nil && body.Model != "" {
				model = body.Model
			}
		}
	}
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	r.Body, _ = r.GetBody()
	r.ContentLength = int64(len(data))
	var route *Model
	for i := range c.Models {
		if c.Models[i].Name == model {
			route = &c.Models[i]
			break
		}
	}
	if route == nil && model != "" {
		for i := range c.Models {
			if c.Models[i].Model == model {
				if route != nil {
					apiError(w, 400, "model matches multiple providers; use a configured model name")
					return
				}
				route = &c.Models[i]
			}
		}
	}
	b.passthrough(w, r, route)
}
