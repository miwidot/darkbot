package builtin

import (
	"context"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/miwidot/darkbot/internal/twitch"
)

// loginPattern: a Twitch login, the only text !followage takes as a name.
var loginPattern = regexp.MustCompile(`^[a-z0-9_]{1,25}$`)

// dateLayouts formats the follow date per language; others use English.
var dateLayouts = map[string]string{"de": "02.01.2006"}

const dateLayoutDefault = "Jan 2, 2006"

// uptime answers how long the stream has been live, read from Twitch
// (Get Streams' started_at) so it is right even if darkbot started later.
func (h *Handler) uptime(ctx context.Context, m twitch.ChatMessage) string {
	live, since, err := h.d.API.LiveSince(ctx, m.BroadcasterID)
	switch {
	case err != nil:
		h.d.Log.Warnf("Uptime for %s failed: %v", m.ChatterName, err)
		return h.t("chat.cmdFailed")
	case !live:
		return h.t("chat.uptimeOffline")
	}
	d := h.d.Now().Sub(since)
	if d < time.Minute {
		return h.t("chat.uptimeJustStarted")
	}
	hours, mins := int(d/time.Hour), int(d%time.Hour/time.Minute)
	return h.t("chat.uptime", "time", h.units(unit{"dur.hour", hours}, unit{"dur.minute", mins}))
}

// followage answers how long the asker, or the account named after the
// command ("!followage @name"), has been following the channel.
func (h *Handler) followage(ctx context.Context, m twitch.ChatMessage) string {
	if r, missing := h.needScope(twitch.ScopeReadFollowers); missing {
		return r
	}
	id, name := m.ChatterID, m.ChatterName
	_, arg, _ := strings.Cut(strings.TrimSpace(m.Text), " ")
	if first, _, _ := strings.Cut(strings.TrimSpace(arg), " "); first != "" {
		login := strings.ToLower(strings.TrimPrefix(first, "@"))
		if !loginPattern.MatchString(login) {
			return h.t("chat.followageUsage", "command", "!"+h.d.Settings(KeyFollowage).Name)
		}
		u, found, err := h.d.API.UserByLogin(ctx, login)
		switch {
		case err != nil:
			h.d.Log.Warnf("Followage: looking up %s failed: %v", login, err)
			return h.t("chat.cmdFailed")
		case !found:
			return h.t("chat.userUnknown", "user", login)
		}
		id, name = u.ID, u.DisplayName
	}
	if id == m.BroadcasterID {
		return h.t("chat.followageSelf", "user", name)
	}
	at, following, err := h.d.API.FollowedAt(ctx, m.BroadcasterID, id)
	switch {
	case err != nil:
		h.d.Log.Warnf("Followage for %s failed: %v", name, err)
		return h.t("chat.cmdFailed")
	case !following:
		return h.t("chat.notFollowing", "user", name)
	}
	now := h.d.Now()
	y, mo, d := calendarSince(at, now)
	date := at.In(now.Location()).Format(h.dateLayout())
	if y == 0 && mo == 0 && d == 0 {
		return h.t("chat.followageToday", "user", name, "date", date)
	}
	return h.t("chat.followage", "user", name, "time",
		h.units(unit{"dur.year", y}, unit{"dur.month", mo}, unit{"dur.day", d}), "date", date)
}

// calendarSince counts whole years, months and days from from to to in
// to's time zone, the way people count them: a month later is the same
// day of the month, or the month's last day when it is shorter (Jan 31 to
// Mar 1 is 1 month and 1 day, Feb 29 to Feb 28 a year later is 1 year).
func calendarSince(from, to time.Time) (years, months, days int) {
	loc := to.Location()
	start, end := noonOf(from.In(loc)), noonOf(to)
	if end.Before(start) { // clock skew
		return 0, 0, 0
	}
	total := (end.Year()-start.Year())*monthsPerYear + int(end.Month()-start.Month())
	if addMonths(start, total).After(end) {
		total--
	}
	// Noon to noon, rounded: a daylight saving change adds or takes an hour.
	days = int(math.Round(end.Sub(addMonths(start, total)).Hours() / hoursPerDay))
	return total / monthsPerYear, total % monthsPerYear, days
}

const (
	monthsPerYear = 12
	hoursPerDay   = 24
)

// noonOf is t's date at noon, far from any midnight a time change shifts.
func noonOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), hoursPerDay/2, 0, 0, 0, t.Location())
}

// addMonths moves t n months on, to the month's last day if t's day is
// past it.
func addMonths(t time.Time, n int) time.Time {
	first := time.Date(t.Year(), t.Month()+time.Month(n), 1, t.Hour(), 0, 0, 0, t.Location())
	last := time.Date(first.Year(), first.Month()+1, 0, t.Hour(), 0, 0, 0, t.Location()).Day()
	return time.Date(first.Year(), first.Month(), min(t.Day(), last), t.Hour(), 0, 0, 0, t.Location())
}

type unit struct {
	key string // i18n key; ".one" and ".other" pick the form
	n   int
}

// maxUnits: "1 year and 3 months" reads well, a third unit is noise.
const maxUnits = 2

// units joins the largest non-zero units, e.g. "2 hours and 5 minutes".
func (h *Handler) units(us ...unit) string {
	var parts []string
	for _, u := range us {
		if u.n == 0 {
			if len(parts) > 0 {
				break // "1 year and 4 days" would skip the months
			}
			continue
		}
		form := ".other"
		if u.n == 1 {
			form = ".one"
		}
		parts = append(parts, h.t(u.key+form, "n", u.n))
		if len(parts) == maxUnits {
			break
		}
	}
	if len(parts) == maxUnits {
		return h.t("dur.and", "a", parts[0], "b", parts[1])
	}
	return strings.Join(parts, "")
}

func (h *Handler) dateLayout() string {
	if l, ok := dateLayouts[h.d.Lang()]; ok {
		return l
	}
	return dateLayoutDefault
}
