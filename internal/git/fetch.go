package git

import (
	"bytes"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

type fetchRequest struct {
	wants       []string
	depth       int
	filtered    bool
	includeTags bool
	incremental bool
}

// Inspect a bounded prefix and restore it even when the request must pass through.
func readFetch(r *http.Request) (fetchRequest, bool) {
	if r.Header.Get("Content-Encoding") != "" {
		return fetchRequest{}, false
	}
	const maxRequest = 1 << 20
	body := r.Body

	data, err := io.ReadAll(io.LimitReader(body, maxRequest+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(data), body), body}
	if err != nil || len(data) > maxRequest {
		return fetchRequest{}, false
	}

	return parseFetch(data)
}

func parseFetch(data []byte) (fetchRequest, bool) {
	var request fetchRequest

	for len(data) > 0 {
		if len(data) < 4 {
			return request, false
		}

		size, err := strconv.ParseUint(string(data[:4]), 16, 16)
		if err != nil || size == 3 || size > 65520 || int(size) > len(data) {
			return request, false
		}
		if size < 4 {
			data = data[4:]
			continue
		}

		line := strings.TrimSuffix(string(data[4:size]), "\n")
		data = data[size:]
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return request, false
		}

		switch fields[0] {
		case "want":
			if len(fields) < 2 || !validObjectID(fields[1]) || slices.Contains(fields[2:], "deepen-relative") {
				return request, false
			}
			request.wants = append(request.wants, fields[1])
			request.includeTags = request.includeTags || slices.Contains(fields[2:], "include-tag")
		case "have", "shallow":
			if len(fields) != 2 || !validObjectID(fields[1]) {
				return request, false
			}
			request.incremental = request.incremental || fields[0] == "have"
		case "deepen":
			if len(fields) != 2 || fields[1] != "1" || request.depth != 0 {
				return request, false
			}
			request.depth = 1
		case "filter":
			request.filtered = true
		case "include-tag":
			request.includeTags = true
		case "command=fetch", "thin-pack", "no-progress", "ofs-delta", "done":
		default:
			if !strings.HasPrefix(line, "agent=") && !strings.HasPrefix(line, "object-format=") {
				return request, false
			}
		}
	}

	slices.Sort(request.wants)
	request.wants = slices.Compact(request.wants)
	return request, len(request.wants) > 0 && (request.depth == 0 || len(request.wants) <= 32)
}
