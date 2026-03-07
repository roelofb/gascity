package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
)

// pageParams holds parsed cursor-based pagination parameters.
type pageParams struct {
	Offset int
	Limit  int
}

// parsePagination extracts cursor and limit from query parameters.
// The cursor is an opaque string that encodes an offset into the result set.
func parsePagination(r *http.Request, defaultLimit int) pageParams {
	q := r.URL.Query()
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	var offset int
	if c := q.Get("cursor"); c != "" {
		offset = decodeCursor(c)
	}
	return pageParams{Offset: offset, Limit: limit}
}

// decodeCursor decodes an opaque cursor string to an integer offset.
// Returns 0 for invalid or empty cursors.
func decodeCursor(cursor string) int {
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(string(data))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// encodeCursor encodes an integer offset as an opaque cursor string.
func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// paginate applies cursor-based pagination to a slice. Returns the page,
// the total count (pre-pagination), and an opaque cursor for the next page
// (empty string if this is the last page).
func paginate[T any](items []T, pp pageParams) (page []T, total int, nextCursor string) {
	total = len(items)
	if pp.Offset >= total {
		return nil, total, ""
	}
	end := pp.Offset + pp.Limit
	if end > total {
		end = total
	}
	page = items[pp.Offset:end]
	if end < total {
		nextCursor = encodeCursor(end)
	}
	return page, total, nextCursor
}
