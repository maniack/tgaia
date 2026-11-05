// Package skills runs database-backed Starlark skills without exposing the
// host filesystem, network, processes, environment or native modules.
package skills

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/storage"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

type Runtime struct{ Store *storage.AgentStore }

type runState struct {
	ctx     context.Context
	store   *storage.AgentStore
	scope   chat.Scope
	userID  int64
	changes map[string]*string
	tmp     map[string]string
	written int
}

func (r Runtime) Run(ctx context.Context, scope chat.Scope, userID int64, source, input string, commit bool) (string, error) {
	if r.Store == nil || !scope.Valid() || userID <= 0 || len(source) > 64000 || len(input) > 64000 {
		return "", errors.New("invalid skill execution")
	}
	state := &runState{ctx: ctx, store: r.Store, scope: scope, userID: userID, changes: map[string]*string{}, tmp: map[string]string{}}
	thread := &starlark.Thread{Name: "database-skill", Print: func(_ *starlark.Thread, _ string) {}}
	thread.SetMaxExecutionSteps(250000)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-ctx.Done():
			once.Do(func() { thread.Cancel("execution deadline exceeded") })
		case <-done:
		}
	}()
	defer close(done)
	predeclared := starlark.StringDict{
		"vfs_read":   starlark.NewBuiltin("vfs_read", state.read),
		"vfs_write":  starlark.NewBuiltin("vfs_write", state.write),
		"vfs_list":   starlark.NewBuiltin("vfs_list", state.list),
		"vfs_delete": starlark.NewBuiltin("vfs_delete", state.delete),
	}
	globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, "skill.star", source, predeclared)
	if err != nil {
		return "", fmt.Errorf("skill compile failed: %w", err)
	}
	fn, ok := globals["run"].(starlark.Callable)
	if !ok {
		return "", errors.New("skill must define run(input_json)")
	}
	value, err := starlark.Call(thread, fn, starlark.Tuple{starlark.String(input)}, nil)
	if err != nil {
		return "", fmt.Errorf("skill run failed: %w", err)
	}
	output, ok := starlark.AsString(value)
	if !ok || len(output) > 64000 {
		return "", errors.New("skill must return a string of at most 64000 bytes")
	}
	changes := make([]storage.VirtualFileChange, 0, len(state.changes))
	for path, content := range state.changes {
		changes = append(changes, storage.VirtualFileChange{Path: path, Content: content})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	if commit {
		if err := r.Store.ApplyVirtualChanges(ctx, scope, userID, changes); err != nil {
			return "", err
		}
	}
	return output, nil
}

func virtualPath(raw string) (string, error) {
	if strings.HasPrefix(raw, "/tmp/") && len(raw) > len("/tmp/") && len(raw) <= 512 && !strings.Contains(raw, "..") {
		return raw, nil
	}
	if len(raw) > 512 || (!strings.HasPrefix(raw, "/user/") && !strings.HasPrefix(raw, "/chat/") && !strings.HasPrefix(raw, "/bot/")) || strings.Contains(raw, "..") {
		return "", errors.New("invalid virtual path")
	}
	return raw, nil
}

func (s *runState) read(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var raw string
	if err := starlark.UnpackArgs("vfs_read", args, kwargs, "path", &raw); err != nil {
		return nil, err
	}
	p, err := virtualPath(raw)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(p, "/tmp/") {
		value, ok := s.tmp[p]
		if !ok {
			return nil, errors.New("virtual file not found")
		}
		return starlark.String(value), nil
	}
	if staged, ok := s.changes[p]; ok {
		if staged == nil {
			return nil, errors.New("virtual file not found")
		}
		return starlark.String(*staged), nil
	}
	file, err := s.store.ReadVirtualFile(s.ctx, s.scope, s.userID, p)
	if err != nil {
		return nil, err
	}
	return starlark.String(file.Content), nil
}

func (s *runState) write(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var raw, content string
	if err := starlark.UnpackArgs("vfs_write", args, kwargs, "path", &raw, "content", &content); err != nil {
		return nil, err
	}
	p, err := virtualPath(raw)
	if err != nil || len(content) > 256000 || s.written+len(content) > 1000000 {
		return nil, errors.New("virtual write exceeds sandbox limits")
	}
	s.written += len(content)
	if strings.HasPrefix(p, "/tmp/") {
		s.tmp[p] = content
	} else {
		copy := content
		s.changes[p] = &copy
	}
	return starlark.None, nil
}

func (s *runState) delete(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var raw string
	if err := starlark.UnpackArgs("vfs_delete", args, kwargs, "path", &raw); err != nil {
		return nil, err
	}
	p, err := virtualPath(raw)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(p, "/tmp/") {
		delete(s.tmp, p)
	} else {
		s.changes[p] = nil
	}
	return starlark.None, nil
}

func (s *runState) list(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var namespace string
	if err := starlark.UnpackArgs("vfs_list", args, kwargs, "namespace", &namespace); err != nil {
		return nil, err
	}
	if namespace == "tmp" {
		paths := make([]string, 0, len(s.tmp))
		for p := range s.tmp {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		values := make([]starlark.Value, len(paths))
		for i, p := range paths {
			values[i] = starlark.String(p)
		}
		return starlark.NewList(values), nil
	}
	rows, err := s.store.ListVirtualFiles(s.ctx, s.scope, s.userID, namespace, "")
	if err != nil {
		return nil, err
	}
	paths := storage.VirtualPaths(rows, namespace)
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	for p, content := range s.changes {
		if strings.HasPrefix(p, "/"+namespace+"/") {
			seen[p] = content != nil
		}
	}
	paths = paths[:0]
	for p, present := range seen {
		if present {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	values := make([]starlark.Value, len(paths))
	for i, p := range paths {
		values[i] = starlark.String(p)
	}
	return starlark.NewList(values), nil
}
