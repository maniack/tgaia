package webui

import (
	"context"
	"maps"
)

func (s *Server) validateSettingChange(ctx context.Context, scope, name string, value *string) error {
	shared, _ := s.Settings.InScope("shared")
	base, _, err := shared.Load(ctx)
	if err != nil {
		return err
	}
	delete(base, "role")
	apply := func(values map[string]string) {
		if value == nil {
			delete(values, name)
		} else {
			values[name] = *value
		}
	}
	if scope == "shared" {
		apply(base)
	}
	for _, role := range []string{"api", "worker"} {
		values := maps.Clone(base)
		settings, _ := s.Settings.InScope(role)
		extra, _, err := settings.Load(ctx)
		if err != nil {
			return err
		}
		if scope == role {
			apply(extra)
		}
		maps.Copy(values, extra)
		if _, err := s.Base.WithSettings(values); err != nil {
			return err
		}
	}
	return nil
}
