package usagestats

import (
	"bytes"
	"compress/gzip"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const responseCompressionLevel = 2

func maybeCompressResponse(request pluginapi.ManagementRequest, response pluginapi.ManagementResponse, config Config) pluginapi.ManagementResponse {
	if !config.ResponseCompression || !strings.HasPrefix(request.Path, "/v0/resource/plugins/") {
		return response
	}
	if !acceptsGzip(request.Headers) || len(response.Body) < config.ResponseCompressionMinBytes {
		return response
	}
	if response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusNotModified || len(response.Body) == 0 {
		return response
	}
	if response.Headers.Get("Content-Encoding") != "" || response.Headers.Get("Content-Range") != "" {
		return response
	}
	if cacheControlDisallowsTransform(response.Headers) || !compressibleContentType(response.Headers.Get("Content-Type")) {
		return response
	}

	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, responseCompressionLevel)
	if err != nil {
		return response
	}
	if _, err = writer.Write(response.Body); err != nil {
		_ = writer.Close()
		return response
	}
	if err = writer.Close(); err != nil {
		return response
	}

	response.Headers = response.Headers.Clone()
	response.Headers.Set("Content-Encoding", "gzip")
	response.Headers.Del("Content-Length")
	appendVary(response.Headers, "Accept-Encoding")
	response.Body = compressed.Bytes()
	return response
}

func acceptsGzip(headers http.Header) bool {
	values := headerValuesFold(headers, "Accept-Encoding")
	if len(values) == 0 {
		return false
	}

	gzipQuality, gzipSet := 0.0, false
	wildcardQuality, wildcardSet := 0.0, false
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			coding, quality, ok := parseEncodingItem(item)
			if !ok {
				continue
			}
			switch coding {
			case "gzip":
				if !gzipSet || quality > gzipQuality {
					gzipQuality = quality
				}
				gzipSet = true
			case "*":
				if !wildcardSet || quality > wildcardQuality {
					wildcardQuality = quality
				}
				wildcardSet = true
			}
		}
	}
	if gzipSet {
		return gzipQuality > 0
	}
	return wildcardSet && wildcardQuality > 0
}

func parseEncodingItem(item string) (string, float64, bool) {
	parts := strings.Split(item, ";")
	coding := strings.ToLower(strings.TrimSpace(parts[0]))
	if coding == "" {
		return "", 0, false
	}
	quality := 1.0
	for _, parameter := range parts[1:] {
		name, value, found := strings.Cut(parameter, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || parsed < 0 || parsed > 1 {
			return coding, 0, true
		}
		quality = parsed
	}
	return coding, quality, true
}

func headerValuesFold(headers http.Header, name string) []string {
	var values []string
	for key, current := range headers {
		if strings.EqualFold(key, name) {
			values = append(values, current...)
		}
	}
	return values
}

func compressibleContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "text/html" || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func cacheControlDisallowsTransform(headers http.Header) bool {
	for _, value := range headerValuesFold(headers, "Cache-Control") {
		for _, directive := range strings.Split(value, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(directive), "=")
			if strings.EqualFold(name, "no-transform") {
				return true
			}
		}
	}
	return false
}

func appendVary(headers http.Header, name string) {
	for _, value := range headerValuesFold(headers, "Vary") {
		for _, current := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(current), name) || strings.TrimSpace(current) == "*" {
				return
			}
		}
	}
	headers.Add("Vary", name)
}
