// The paging query shape that the paginated repo-answer routes share.

package api

import (
	"net/url"
	"strconv"
)

// parsePagingShape canonicalizes per_page and page into one row key. Any other parameter, a repeat, or a value out of range reports false.
func parsePagingShape(q url.Values, defaultPerPage, maxPage int) (string, bool) {
	perPage, page := defaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return "", false
		}
		n, err := strconv.Atoi(vals[0])
		switch key {
		case "per_page":
			if err != nil || n < 1 || n > 100 {
				return "", false
			}
			perPage = n
		case "page":
			if err != nil || n < 1 || n > maxPage {
				return "", false
			}
			page = n
		default:
			return "", false
		}
	}
	return "per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page), true
}
