package fyneui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"blat/internal/config"
	"blat/internal/core"
	"blat/internal/runtime"
)

// fwContentA/fwContentB 是固件测试文件的两个版本内容（A 先写盘，B 用于
// "外部替换文件不影响已生成快照"的验证）。
var (
	fwContentA = []byte("FAKE-FIRMWARE-V1-AAAAAAAAAAAAAAAA")
	fwContentB = []byte("FAKE-FIRMWARE-V2-BBBBBBBBBBBBBBBB")
)

// writeFWFile 在 t.TempDir() 下写一个固件文件并返回路径。
func writeFWFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写固件文件失败: %v", err)
	}
	return p
}

// upgradePlan 构造含升级用例的 plan（可指定 plan 参数与 counts）。
func upgradePlan(t *testing.T, args map[string]any) *config.Plan {
	t.Helper()
	return &config.Plan{Cases: []config.CaseItem{{
		Name: upgradeFirmwareCaseName,
		Args: args,
	}}}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ---------- firmwarePathForPlan（路径解析与 case 一致） ----------

func TestFirmwarePathForPlan(t *testing.T) {
	env := &core.Env{Vars: map[string]any{
		"HeatNote": map[string]any{
			"mbus": map[string]any{"port": "COM9", "firmware": `C:\base\fw.bin`},
		},
	}}

	t.Run("英文 firmware 空串视作未配置，回退 HeatNote", func(t *testing.T) {
		plan := upgradePlan(t, map[string]any{"firmware": ""})
		path, need := firmwarePathForPlan(plan, env)
		if !need || path != `C:\base\fw.bin` {
			t.Errorf("firmware:空串应回退 HeatNote.mbus.firmware, got (%q, %v)", path, need)
		}
	})

	t.Run("英文 firmware 空串且 HeatNote 未配置", func(t *testing.T) {
		plan := upgradePlan(t, map[string]any{"firmware": ""})
		envNoFw := &core.Env{Vars: map[string]any{"HeatNote": map[string]any{"mbus": map[string]any{"port": "COM9"}}}}
		path, need := firmwarePathForPlan(plan, envNoFw)
		if !need || path != "" {
			t.Errorf("firmware:空串且无 HeatNote 应返回 (\"\", true), got (%q, %v)", path, need)
		}
	})

	t.Run("升级文件中文参数优先", func(t *testing.T) {
		plan := upgradePlan(t, map[string]any{"升级文件": `C:\cn\fw.bin`, "firmware": `C:\en\fw.bin`})
		path, need := firmwarePathForPlan(plan, env)
		if !need || path != `C:\cn\fw.bin` {
			t.Errorf("firmwarePathForPlan = (%q, %v), want (C:\\cn\\fw.bin, true)", path, need)
		}
	})

	t.Run("英文 firmware 参数次之", func(t *testing.T) {
		plan := upgradePlan(t, map[string]any{"firmware": `C:\en\fw.bin`})
		path, need := firmwarePathForPlan(plan, env)
		if !need || path != `C:\en\fw.bin` {
			t.Errorf("firmwarePathForPlan = (%q, %v), want (C:\\en\\fw.bin, true)", path, need)
		}
	})

	t.Run("无参数回退 HeatNote.mbus.firmware", func(t *testing.T) {
		plan := upgradePlan(t, nil)
		path, need := firmwarePathForPlan(plan, env)
		if !need || path != `C:\base\fw.bin` {
			t.Errorf("firmwarePathForPlan = (%q, %v), want (C:\\base\\fw.bin, true)", path, need)
		}
	})

	t.Run("升级 plan 但完全未配置路径", func(t *testing.T) {
		plan := upgradePlan(t, nil)
		envNoFw := &core.Env{Vars: map[string]any{"HeatNote": map[string]any{"mbus": map[string]any{"port": "COM9"}}}}
		path, need := firmwarePathForPlan(plan, envNoFw)
		if !need || path != "" {
			t.Errorf("firmwarePathForPlan = (%q, %v), want (\"\", true)", path, need)
		}
	})

	t.Run("非升级 plan 不需要快照", func(t *testing.T) {
		plan := &config.Plan{Cases: []config.CaseItem{{Name: "HeatSuite::wire_valve_mbus_read_motor"}}}
		path, need := firmwarePathForPlan(plan, env)
		if need || path != "" {
			t.Errorf("firmwarePathForPlan = (%q, %v), want (\"\", false)", path, need)
		}
	})

	t.Run("nil plan/env 返回不需要", func(t *testing.T) {
		if _, need := firmwarePathForPlan(nil, env); need {
			t.Error("nil plan 不应需要固件")
		}
		if _, need := firmwarePathForPlan(upgradePlan(t, nil), nil); need {
			t.Error("nil env 不应需要固件")
		}
	})
}

// ---------- loadFirmwareSnapshot ----------

func TestLoadFirmwareSnapshotCreateAndReuse(t *testing.T) {
	a := newPanelTestApp(t)
	fwPath := writeFWFile(t, "fw.bin", fwContentA)
	a.plan = upgradePlan(t, map[string]any{"升级文件": fwPath})

	// 首次：读盘 + 哈希
	a.mu.Lock()
	if err := a.loadFirmwareSnapshot(); err != nil {
		a.mu.Unlock()
		t.Fatalf("loadFirmwareSnapshot: %v", err)
	}
	first := a.fwSnap
	a.mu.Unlock()

	if first == nil {
		t.Fatal("升级 plan 应生成快照")
	}
	if first.Path != fwPath {
		t.Errorf("Path = %q, want %q", first.Path, fwPath)
	}
	if string(first.Data) != string(fwContentA) {
		t.Errorf("Data 与文件内容不一致")
	}
	if first.Size != int64(len(fwContentA)) {
		t.Errorf("Size = %d, want %d", first.Size, len(fwContentA))
	}
	if want := sha256Hex(fwContentA); first.SHA256 != want {
		t.Errorf("SHA256 = %q, want %q", first.SHA256, want)
	}

	// 二次：同 plan 同路径直接复用（同一指针，不重新读盘）
	a.mu.Lock()
	if err := a.loadFirmwareSnapshot(); err != nil {
		a.mu.Unlock()
		t.Fatalf("第二次 loadFirmwareSnapshot: %v", err)
	}
	second := a.fwSnap
	a.mu.Unlock()
	if second != first {
		t.Error("同 plan 同路径应复用同一快照指针")
	}
}

func TestLoadFirmwareSnapshotFileReplacedKeepsSnapshot(t *testing.T) {
	a := newPanelTestApp(t)
	fwPath := writeFWFile(t, "fw.bin", fwContentA)
	a.plan = upgradePlan(t, map[string]any{"升级文件": fwPath})

	a.mu.Lock()
	if err := a.loadFirmwareSnapshot(); err != nil {
		a.mu.Unlock()
		t.Fatalf("loadFirmwareSnapshot: %v", err)
	}
	first := a.fwSnap
	a.mu.Unlock()

	// 外部替换文件：快照必须保持原内容/哈希（不重新读盘）
	if err := os.WriteFile(fwPath, fwContentB, 0o644); err != nil {
		t.Fatalf("替换固件文件失败: %v", err)
	}
	a.mu.Lock()
	if err := a.loadFirmwareSnapshot(); err != nil {
		a.mu.Unlock()
		t.Fatalf("替换文件后 loadFirmwareSnapshot: %v", err)
	}
	second := a.fwSnap
	a.mu.Unlock()
	if second != first {
		t.Error("外部替换文件后应仍复用同一快照指针")
	}
	if string(second.Data) != string(fwContentA) {
		t.Error("外部替换文件后快照 Data 不应变化")
	}
	if second.SHA256 != sha256Hex(fwContentA) {
		t.Error("外部替换文件后快照 SHA256 不应变化")
	}
}

func TestLoadFirmwareSnapshotPlanOrPathChangeRebuilds(t *testing.T) {
	a := newPanelTestApp(t)
	fwPathA := writeFWFile(t, "fwA.bin", fwContentA)
	a.plan = upgradePlan(t, map[string]any{"升级文件": fwPathA})

	a.mu.Lock()
	if err := a.loadFirmwareSnapshot(); err != nil {
		a.mu.Unlock()
		t.Fatalf("loadFirmwareSnapshot: %v", err)
	}
	first := a.fwSnap
	a.mu.Unlock()

	// 路径变化：必须重建，不能误用旧快照
	fwPathB := writeFWFile(t, "fwB.bin", fwContentB)
	a.plan = upgradePlan(t, map[string]any{"升级文件": fwPathB})
	a.mu.Lock()
	if err := a.loadFirmwareSnapshot(); err != nil {
		a.mu.Unlock()
		t.Fatalf("路径变化后 loadFirmwareSnapshot: %v", err)
	}
	rebuilt := a.fwSnap
	a.mu.Unlock()
	if rebuilt == first {
		t.Error("路径变化应重建快照（指针应不同）")
	}
	if string(rebuilt.Data) != string(fwContentB) || rebuilt.SHA256 != sha256Hex(fwContentB) {
		t.Error("重建后的快照应读取新路径内容")
	}

	// plan 变化（同路径）：也必须重建
	a.plan = upgradePlan(t, map[string]any{"升级文件": fwPathB, "软件版本": 5})
	a.mu.Lock()
	if err := a.loadFirmwareSnapshot(); err != nil {
		a.mu.Unlock()
		t.Fatalf("plan 变化后 loadFirmwareSnapshot: %v", err)
	}
	rebuilt2 := a.fwSnap
	a.mu.Unlock()
	if rebuilt2 == rebuilt {
		t.Error("plan 变化应重建快照（指针应不同）")
	}
}

func TestLoadFirmwareSnapshotErrors(t *testing.T) {
	t.Run("文件不存在返回错误并清缓存", func(t *testing.T) {
		a := newPanelTestApp(t)
		fwPath := writeFWFile(t, "fw.bin", fwContentA)
		a.plan = upgradePlan(t, map[string]any{"升级文件": fwPath})
		a.mu.Lock()
		if err := a.loadFirmwareSnapshot(); err != nil {
			a.mu.Unlock()
			t.Fatalf("首次 loadFirmwareSnapshot: %v", err)
		}
		a.mu.Unlock()

		a.plan = upgradePlan(t, map[string]any{"升级文件": filepath.Join(filepath.Dir(fwPath), "missing.bin")})
		a.mu.Lock()
		err := a.loadFirmwareSnapshot()
		if err == nil {
			a.mu.Unlock()
			t.Fatal("文件不存在应返回错误")
		}
		if a.fwSnap != nil {
			a.mu.Unlock()
			t.Error("读取失败后应清空快照缓存，不能误用旧快照")
		}
		a.mu.Unlock()
	})

	t.Run("升级 plan 未配置路径返回错误", func(t *testing.T) {
		a := newPanelTestApp(t)
		a.plan = upgradePlan(t, nil) // 无参数、env 无 firmware
		a.mu.Lock()
		err := a.loadFirmwareSnapshot()
		a.mu.Unlock()
		if err == nil {
			t.Fatal("升级 plan 未配置固件应返回错误")
		}
	})

	t.Run("非普通文件返回错误", func(t *testing.T) {
		a := newPanelTestApp(t)
		dir := filepath.Join(t.TempDir(), "fwdir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		a.plan = upgradePlan(t, map[string]any{"升级文件": dir})
		a.mu.Lock()
		err := a.loadFirmwareSnapshot()
		a.mu.Unlock()
		if err == nil {
			t.Fatal("目录作为固件路径应返回错误")
		}
	})
}

func TestLoadFirmwareSnapshotNonUpgradeClears(t *testing.T) {
	a := newPanelTestApp(t)
	fwPath := writeFWFile(t, "fw.bin", fwContentA)
	a.plan = upgradePlan(t, map[string]any{"升级文件": fwPath})
	a.mu.Lock()
	if err := a.loadFirmwareSnapshot(); err != nil {
		a.mu.Unlock()
		t.Fatalf("loadFirmwareSnapshot: %v", err)
	}
	if a.fwSnap == nil {
		a.mu.Unlock()
		t.Fatal("升级 plan 应生成快照")
	}
	a.mu.Unlock()

	// 切到非升级 plan：缓存清空
	a.plan = &config.Plan{Cases: []config.CaseItem{{Name: "HeatSuite::wire_valve_mbus_read_motor"}}}
	a.mu.Lock()
	if err := a.loadFirmwareSnapshot(); err != nil {
		a.mu.Unlock()
		t.Fatalf("非升级 plan loadFirmwareSnapshot: %v", err)
	}
	if a.fwSnap != nil {
		a.mu.Unlock()
		t.Error("非升级 plan 不应保留固件快照")
	}
	a.mu.Unlock()
}

// ---------- startStation 集成 ----------

func TestStartStationSharedFirmwareSnapshot(t *testing.T) {
	a := newPanelTestApp(t)
	a.SetDebug(true)
	fwPath := writeFWFile(t, "fw.bin", fwContentA)
	a.plan = upgradePlan(t, map[string]any{"升级文件": fwPath})
	a.env.Vars["HeatNote"] = map[string]any{"mbus": map[string]any{"port": "COM9"}}
	a.reg = runtime.NewRegistry()

	// 三个工位依次启动：第一个在锁内读盘生成快照，后两个复用同一快照
	for i := 1; i <= 3; i++ {
		if err := a.startStation(i, fmtSN(i), "COM9"); err != nil {
			t.Fatalf("startStation(%d): %v", i, err)
		}
	}

	// 三个工位拿到同一快照：指针/字节/hash 一致
	got := a.fwSnap
	if got == nil {
		t.Fatal("三工位启动后应有共享固件快照")
	}
	if got.Path != fwPath || string(got.Data) != string(fwContentA) || got.SHA256 != sha256Hex(fwContentA) {
		t.Errorf("快照内容不对: path=%q sha=%q", got.Path, got.SHA256)
	}

	// 外部替换文件后再次启动：仍复用原快照（不重新读盘）
	if err := os.WriteFile(fwPath, fwContentB, 0o644); err != nil {
		t.Fatalf("替换固件文件失败: %v", err)
	}
	// 等前一轮 bootStation 清空占位槽后再启动工位1（同槽位二次启动）
	waitStationSlotFree(t, a, 1)
	if err := a.startStation(1, fmtSN(11), "COM9"); err != nil {
		t.Fatalf("替换文件后 startStation(1): %v", err)
	}
	if a.fwSnap != got {
		t.Error("替换文件后应复用同一快照指针")
	}
	if a.fwSnap.SHA256 != sha256Hex(fwContentA) {
		t.Error("替换文件后快照 hash 应保持原值")
	}

	// 收尾：停止工位并等待 bootStation goroutine 退出
	a.stopAllStations()
	a.stationWG.Wait()
}

func TestStartStationUpgradeFirmwareReadFailureNoSlot(t *testing.T) {
	a := newPanelTestApp(t)
	a.SetDebug(true)
	a.plan = upgradePlan(t, map[string]any{"升级文件": filepath.Join(t.TempDir(), "missing.bin")})
	a.env.Vars["HeatNote"] = map[string]any{"mbus": map[string]any{"port": "COM9"}}
	a.reg = runtime.NewRegistry()

	err := a.startStation(1, fmtSN(1), "COM9")
	if err == nil {
		t.Fatal("升级 plan 固件读取失败应返回错误")
	}
	if a.stationRuns[0] != nil {
		t.Error("固件读取失败不应登记占位（不启动任何槽位）")
	}
	if a.fwSnap != nil {
		t.Error("固件读取失败不应留下快照缓存")
	}
}

func TestStartStationNonUpgradePlanNoFirmwareRequired(t *testing.T) {
	a := newPanelTestApp(t)
	a.SetDebug(true)
	// 非升级 plan：完全不配置固件文件也能启动
	a.plan = &config.Plan{Cases: []config.CaseItem{{Name: "HeatSuite::wire_valve_mbus_read_motor"}}}
	a.env.Vars["HeatNote"] = map[string]any{"mbus": map[string]any{"port": "COM9"}}
	a.reg = runtime.NewRegistry()

	if err := a.startStation(1, fmtSN(1), "COM9"); err != nil {
		t.Fatalf("非升级 plan startStation 应成功: %v", err)
	}
	if a.fwSnap != nil {
		t.Error("非升级 plan 不应要求/生成固件快照")
	}

	a.stopAllStations()
	a.stationWG.Wait()
}

// ---------- newStationEnv 接线 ----------

func TestNewStationEnvFirmwareWiring(t *testing.T) {
	fwPath := writeFWFile(t, "fw.bin", fwContentA)
	snap := &core.FirmwareSnapshot{
		Path:   fwPath,
		Data:   fwContentA,
		Size:   int64(len(fwContentA)),
		SHA256: sha256Hex(fwContentA),
	}
	env := newStationEnv(context.Background(), nil, nil, nil, nil, nil, snap)
	if env.Firmware != snap {
		t.Error("senv.Firmware 应指向传入的共享快照（同一指针）")
	}

	envNil := newStationEnv(context.Background(), nil, nil, nil, nil, nil, nil)
	if envNil.Firmware != nil {
		t.Error("非升级 plan 场景 senv.Firmware 应为 nil")
	}
}

// ---------- 清缓存点 ----------

func TestApplyConfigClearsFirmwareSnapshot(t *testing.T) {
	a := newPanelTestApp(t)
	a.varsFile = filepath.Join(t.TempDir(), "env.yml")
	fwPath := writeFWFile(t, "fw.bin", fwContentA)
	a.plan = upgradePlan(t, map[string]any{"升级文件": fwPath})
	a.env.Vars["HeatNote"] = map[string]any{"mbus": map[string]any{"port": "COM5"}}

	a.mu.Lock()
	if err := a.loadFirmwareSnapshot(); err != nil {
		a.mu.Unlock()
		t.Fatalf("loadFirmwareSnapshot: %v", err)
	}
	if a.fwSnap == nil {
		a.mu.Unlock()
		t.Fatal("应先生成快照")
	}
	a.mu.Unlock()

	// 配置保存成功后清空快照缓存（含 Data 引用）
	a.applyConfig("COM9", "")
	if a.fwSnap != nil {
		t.Error("applyConfig 成功后应清空固件快照缓存")
	}
}

func TestStopAllStationsClearsFirmwareSnapshot(t *testing.T) {
	a := newPanelTestApp(t)
	fwPath := writeFWFile(t, "fw.bin", fwContentA)
	a.plan = upgradePlan(t, map[string]any{"升级文件": fwPath})
	a.mu.Lock()
	if err := a.loadFirmwareSnapshot(); err != nil {
		a.mu.Unlock()
		t.Fatalf("loadFirmwareSnapshot: %v", err)
	}
	if a.fwSnap == nil {
		a.mu.Unlock()
		t.Fatal("应先生成快照")
	}
	a.mu.Unlock()

	a.stopAllStations()
	if a.fwSnap != nil {
		t.Error("关窗（stopAllStations）应清空固件快照引用")
	}
}

// fmtSN 生成 12 位数字序列号（面板 SN 校验要求）。
func fmtSN(n int) string {
	s := fmt.Sprintf("%012d", n)
	return s[len(s)-12:]
}

// waitStationSlotFree 轮询等待第 idx 号工位槽位被清空（测试环境里 bootStation
// 在 panel==nil 处快速放弃并清占位）。bootStation 为异步 goroutine，同槽位
// 二次启动前必须等它清完。
func waitStationSlotFree(t *testing.T, a *App, idx int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		busy := a.stationRuns[idx-1] != nil
		a.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("工位%d 槽位在超时内未被清空", idx)
}
