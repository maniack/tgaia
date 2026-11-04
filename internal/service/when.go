package service

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// When is the structured first-run specification a model supplies when it
// creates or updates a task. The model names the user's words (a weekday, a
// date, a wall-clock time, a timezone); the code turns them into an instant.
// Models reliably extract these fields but unreliably add offsets and count
// days, which is exactly the arithmetic this type keeps out of their hands.
type When struct {
	Date         string `json:"date"`          // "YYYY-MM-DD" in the given timezone, or "".
	Weekday      string `json:"weekday"`       // "monday".."sunday", or "".
	Time         string `json:"time"`          // "HH:MM" wall-clock in the given timezone, or "".
	Timezone     string `json:"timezone"`      // IANA name ("Europe/Moscow") or "+03:00"; required with any of the above.
	DelaySeconds int64  `json:"delay_seconds"` // Relative delay; only when no calendar fields are given.
}

const (
	minDelay = 60
	maxDelay = 366 * 24 * 60 * 60
)

var (
	weekdays   = map[string]time.Weekday{"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday, "thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday}
	offsetZone = regexp.MustCompile(`^(?:UTC)?([+-])(\d{1,2})(?::?(\d{2}))?$`)
)

// location accepts IANA names and fixed offsets. Fixed offsets exist because a
// user may say "UTC+3" rather than a city, not because they are preferred.
func location(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("timezone is required")
	}
	if strings.EqualFold(name, "UTC") || name == "Z" {
		return time.UTC, nil
	}
	if m := offsetZone.FindStringSubmatch(name); m != nil {
		hours, _ := strconv.Atoi(m[2])
		minutes := 0
		if m[3] != "" {
			minutes, _ = strconv.Atoi(m[3])
		}
		if hours > 14 || minutes > 59 {
			return nil, errors.New("timezone offset out of range")
		}
		offset := hours*3600 + minutes*60
		if m[1] == "-" {
			offset = -offset
		}
		return time.FixedZone(name, offset), nil
	}
	if strings.EqualFold(name, "Local") || !strings.Contains(name, "/") && !strings.EqualFold(name, "GMT") {
		// Abbreviations such as "MSK" or "EST" are ambiguous and not portable.
		return nil, errors.New("unknown timezone; use an IANA name such as Europe/Moscow")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, errors.New("unknown timezone; use an IANA name such as Europe/Moscow")
	}
	return loc, nil
}

// resolve computes the first run instant from now. It never silently moves a
// requested time: a calendar date in the past is an error the model must
// resolve with the user, while a weekday or bare time rolls to the next
// occurrence, because that is what "on Thursday at 19:00" means.
func (w When) resolve(now time.Time) (time.Time, *time.Location, error) {
	calendar := strings.TrimSpace(w.Date) != "" || strings.TrimSpace(w.Weekday) != "" || strings.TrimSpace(w.Time) != ""
	if !calendar {
		if w.DelaySeconds < minDelay || w.DelaySeconds > maxDelay {
			return time.Time{}, nil, errors.New("delay_seconds must be between 60 and 31622400 when no date, weekday or time is given")
		}
		return now.Add(time.Duration(w.DelaySeconds) * time.Second), time.UTC, nil
	}
	if w.DelaySeconds != 0 {
		return time.Time{}, nil, errors.New("delay_seconds cannot be combined with date, weekday or time")
	}
	loc, err := location(w.Timezone)
	if err != nil {
		return time.Time{}, nil, err
	}
	hour, minute, err := clock(w.Time)
	if err != nil {
		return time.Time{}, nil, err
	}
	local := now.In(loc)
	var at time.Time
	switch {
	case strings.TrimSpace(w.Date) != "":
		day, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(w.Date), loc)
		if err != nil {
			return time.Time{}, nil, errors.New("date must be YYYY-MM-DD")
		}
		at = time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, loc)
		if strings.TrimSpace(w.Weekday) != "" {
			want, ok := weekdays[strings.ToLower(strings.TrimSpace(w.Weekday))]
			if !ok {
				return time.Time{}, nil, errors.New("weekday must be an English day name")
			}
			if at.Weekday() != want {
				return time.Time{}, nil, fmt.Errorf("%s is a %s, not a %s; confirm the date with the user", at.Format("2006-01-02"), strings.ToLower(at.Weekday().String()), strings.ToLower(want.String()))
			}
		}
		if at.Before(now.Add(-time.Minute)) {
			return time.Time{}, nil, errors.New("that date and time is already in the past")
		}
	case strings.TrimSpace(w.Weekday) != "":
		want, ok := weekdays[strings.ToLower(strings.TrimSpace(w.Weekday))]
		if !ok {
			return time.Time{}, nil, errors.New("weekday must be an English day name")
		}
		at = time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
		for i := 0; i < 7 && (at.Weekday() != want || !at.After(now.Add(minDelay*time.Second))); i++ {
			at = at.AddDate(0, 0, 1)
		}
	default:
		at = time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
		if !at.After(now.Add(minDelay * time.Second)) {
			at = at.AddDate(0, 0, 1)
		}
	}
	if !at.After(now.Add(minDelay * time.Second)) {
		at = now.Add(minDelay * time.Second)
	}
	if at.After(now.Add(maxDelay * time.Second)) {
		return time.Time{}, nil, errors.New("first run is more than a year away")
	}
	return at, loc, nil
}

func clock(value string) (int, int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0, errors.New("time is required (HH:MM); ask the user if unknown")
	}
	t, err := time.Parse("15:04", value)
	if err != nil {
		t, err = time.Parse("15:04:05", value)
	}
	if err != nil {
		return 0, 0, errors.New("time must be HH:MM")
	}
	return t.Hour(), t.Minute(), nil
}

// describeRun formats an instant for the user in a given location: the full
// date, the weekday and the zone, so a wrong day cannot hide behind a right hour.
func describeRun(at time.Time, loc *time.Location) string {
	local := at.In(loc)
	zone := loc.String()
	if zone == "" || zone == "Local" {
		zone = local.Format("-07:00")
	}
	return fmt.Sprintf("%s %s %s (%s)", local.Weekday().String()[:3], local.Format("2006-01-02"), local.Format("15:04"), zone)
}

// describeInterval renders a recurrence as the largest exact unit.
func describeInterval(seconds int64) string {
	switch {
	case seconds <= 0:
		return "once"
	case seconds%(7*86400) == 0:
		return plural(seconds/(7*86400), "week")
	case seconds%86400 == 0:
		return plural(seconds/86400, "day")
	case seconds%3600 == 0:
		return plural(seconds/3600, "hour")
	default:
		return plural(seconds/60, "minute")
	}
}

func plural(n int64, unit string) string {
	if n == 1 {
		return "every " + unit
	}
	return fmt.Sprintf("every %d %ss", n, unit)
}
