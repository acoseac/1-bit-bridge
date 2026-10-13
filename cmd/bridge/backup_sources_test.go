package main

import (
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// The CLI, the serve ticker and the console's Snapshot now all call
// buildBackupSources. The restore command calls buildRestoreTargets.
// Both have to name the console account and the harvest state, or a
// snapshot taken on one path restores without them.
func TestBuildBackupSourcesNamesTheConsoleAccountAndHarvestState(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: filepath.Join(dir, "data")}
	yamlPath := filepath.Join(dir, "bridge.yaml")
	src := buildBackupSources(cfg, yamlPath)
	dst := buildRestoreTargets(cfg, yamlPath)

	wantAuth := filepath.Join(cfg.DataDir, adminauth.FileName)
	wantHarvest := harvestStatePath(cfg.DataDir)
	if src.AdminAuth != wantAuth || dst.AdminAuth != wantAuth {
		t.Fatalf("adminauth src %q dst %q, want %q", src.AdminAuth, dst.AdminAuth, wantAuth)
	}
	if src.AtlasHarvest != wantHarvest || dst.AtlasHarvest != wantHarvest {
		t.Fatalf("harvest src %q dst %q, want %q", src.AtlasHarvest, dst.AtlasHarvest, wantHarvest)
	}
	if backup.AdminAuthFileName != adminauth.FileName {
		t.Fatalf("backup names %q, the store names %q", backup.AdminAuthFileName, adminauth.FileName)
	}
	if filepath.Base(src.AtlasHarvest) != backup.AtlasHarvestFileName {
		t.Fatalf("harvest path %q, snapshot name %q", src.AtlasHarvest, backup.AtlasHarvestFileName)
	}
}
