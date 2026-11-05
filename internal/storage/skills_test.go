package storage

import (
	"context"
	"testing"

	"github.com/maniack/tgaia/internal/config"
)

func TestSkillVersionsAndActivation(t *testing.T) {
	db, _ := databasesAtVersion(t, config.SQLite, 23)
	ctx := context.Background()
	first, err := db.Agent().SaveSkill(ctx, "test", 1, 7, "daily_brief", "Build a concise brief", `def run(input_json): return input_json`)
	if err != nil || first.Version != 1 || first.Status != "draft" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if err := db.Agent().FinishSkillTest(ctx, first, `{}`, `ok`, ""); err != nil {
		t.Fatal(err)
	}
	second, err := db.Agent().SaveSkill(ctx, "test", 1, 7, "daily_brief", "Build a better brief", `def run(input_json): return "v2"`)
	if err != nil || second.Version != 2 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	active, err := db.Agent().Skill(ctx, "test", 1, "daily_brief", true)
	if err != nil || active.Version != 1 {
		t.Fatalf("draft replaced active skill: %+v %v", active, err)
	}
	if err := db.Agent().FinishSkillTest(ctx, second, `{}`, `v2`, ""); err != nil {
		t.Fatal(err)
	}
	active, _ = db.Agent().Skill(ctx, "test", 1, "daily_brief", true)
	versions, _ := db.Agent().SkillVersions(ctx, "test", 1, "daily_brief")
	if active.Version != 2 || len(versions) != 2 || versions[1].Status != "draft" {
		t.Fatalf("activation/versions failed: active=%+v versions=%+v", active, versions)
	}
}
