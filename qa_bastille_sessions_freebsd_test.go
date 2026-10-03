//go:build freebsd

package main

import (
	"strings"
	"testing"
)

// TestBastilleSessionIDsSurviveRestart 确保重建会话表后不会复用旧会话 ID 覆盖日志。
func TestBastilleSessionIDsSurviveRestart(t *testing.T) {
	before := &sessionStore{sessions: map[string]*bastilleSession{}}
	after := &sessionStore{sessions: map[string]*bastilleSession{}}
	seen := map[string]bool{}
	for _, store := range []*sessionStore{before, after} {
		for i := 0; i < 100; i++ {
			id := store.create()
			if !strings.HasPrefix(id, "s-") || seen[id] {
				t.Fatalf("会话 ID 为空或重用: %q", id)
			}
			seen[id] = true
		}
	}
}
