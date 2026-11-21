package twitch

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Get Users (https://dev.twitch.tv/docs/api/reference/users, checked
// 2026-11-21) looks accounts up by "login" and needs no scope; an unknown
// login is left out of the list, so an empty list means it doesn't exist.

// User is an account found by login.
type User struct {
	ID          string `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"display_name"`
}

// UserByLogin looks up one account; found is false when no account has
// that login.
func (h *Helix) UserByLogin(ctx context.Context, login string) (u User, found bool, err error) {
	var out struct {
		Data []User `json:"data"`
	}
	q := url.Values{"login": {strings.ToLower(login)}}
	if err := h.do(ctx, http.MethodGet, "/users", q, nil, &out); err != nil {
		return User{}, false, err
	}
	if len(out.Data) == 0 {
		return User{}, false, nil
	}
	return out.Data[0], true, nil
}
