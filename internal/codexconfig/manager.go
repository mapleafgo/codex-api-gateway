package codexconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	providerID   = "codex-api-gateway"
	providerName = "Codex API Gateway"
	wireAPI      = "responses"
	authCommand  = "echo codex-local"

	backupFileName = "codex-api-gateway-backup.json"
	modelsFileName = "models.json"
)

// providerHeader 是我们要整体覆盖的表头。
var providerHeader = "[model_providers." + providerID + "]"

// backupState 记录启用前的 model_provider 原值，null 表示原文件无该键。
type backupState struct {
	ModelProvider    *string `json:"model_provider"`
	ModelCatalogJSON *string `json:"model_catalog_json"`
}

// Manager 管理 Codex「应用 Codex」开关：修改 $CODEX_HOME/config.toml 的
// model_provider 与 codex-api-gateway provider 块。方法并发安全。
type Manager struct {
	baseURL func() string
	mu      sync.Mutex
}

// New 创建 Manager。baseURL 返回网关 base URL（含 /v1），
// 为空时 Enable 返回明确错误，IsEnabled 视为未启用。
func New(baseURL func() string) *Manager {
	return &Manager{baseURL: baseURL}
}

// IsEnabled 报告 Codex 是否已指向本网关：model_provider 为 codex-api-gateway
// 且块内 base_url 与当前网关地址一致。config.toml 缺失时返回 (false, nil)。
func (m *Manager) IsEnabled() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	home, path, err := resolveConfigPaths()
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("codexconfig: 读取 %s 失败: %w", path, err)
	}
	return isEnabled(raw, m.currentBaseURL(), modelsCatalogPath(home)), nil
}

// CatalogPath 返回 Codex 模型目录文件 models.json 的绝对路径。
func (m *Manager) CatalogPath() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	home, _, err := resolveConfigPaths()
	if err != nil {
		return "", err
	}
	return modelsCatalogPath(home), nil
}

// WriteModelsCatalog 把模型目录 JSON 原子写入 $CODEX_HOME/models.json。
// 目录或配置缺失时返回错误，不自动创建 Codex 配置目录。
func (m *Manager) WriteModelsCatalog(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	home, _, err := resolveConfigPaths()
	if err != nil {
		return err
	}
	return writeBytes(modelsCatalogPath(home), data)
}

// Enable 把 Codex 指向本网关：备份原 model_provider、整体覆盖 provider 块、
// 设置 model_provider，并写入 model_catalog_json 指向 models.json。
// config.toml 缺失时返回错误，不自建文件。
func (m *Manager) Enable() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	base := m.currentBaseURL()
	if base == "" {
		return errors.New("codexconfig: 网关地址尚未就绪")
	}
	home, path, err := resolveConfigPaths()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("codexconfig: 未找到 %s，请先运行一次 codex 生成配置", path)
		}
		return fmt.Errorf("codexconfig: 读取 %s 失败: %w", path, err)
	}
	catalogPath := modelsCatalogPath(home)
	if isEnabled(raw, base, catalogPath) {
		return nil
	}
	lines := strings.Split(string(raw), "\n")

	// 原 model_provider 已是我们的值时不再写备份，避免丢失真正的原值。
	if value, ok := topLevelKey(lines, "model_provider"); !ok || value != providerID {
		if err := writeBackup(filepath.Join(home, backupFileName), lines); err != nil {
			return err
		}
	}

	lines = upsertTableBlock(lines, providerHeader, providerBlock(base))
	lines = upsertTopLevelKey(lines, "model_provider", providerID)
	lines = upsertTopLevelKey(lines, "model_catalog_json", catalogPath)
	return writeConfig(path, lines)
}

// Disable 恢复启用前的 model_provider 原值并删除备份；provider 块保留。
func (m *Manager) Disable() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	home, path, err := resolveConfigPaths()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // config.toml 缺失：no-op，不创建文件
		}
		return fmt.Errorf("codexconfig: 读取 %s 失败: %w", path, err)
	}
	backupPath := filepath.Join(home, backupFileName)
	data, err := os.ReadFile(backupPath)
	if err != nil {
		if os.IsNotExist(err) {
			lines := strings.Split(string(raw), "\n")
			if value, ok := topLevelKey(lines, "model_provider"); ok && value == providerID {
				return fmt.Errorf("codexconfig: 备份文件 %s 缺失，无法恢复，保持现状", backupPath)
			}
			return nil
		}
		return fmt.Errorf("codexconfig: 读取备份 %s 失败: %w", backupPath, err)
	}
	var state backupState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("codexconfig: 解析备份 %s 失败: %w", backupPath, err)
	}
	lines := strings.Split(string(raw), "\n")
	if state.ModelProvider == nil {
		lines = removeTopLevelKey(lines, "model_provider")
	} else {
		lines = upsertTopLevelKey(lines, "model_provider", *state.ModelProvider)
	}
	if state.ModelCatalogJSON == nil {
		lines = removeTopLevelKey(lines, "model_catalog_json")
	} else {
		lines = upsertTopLevelKey(lines, "model_catalog_json", *state.ModelCatalogJSON)
	}
	if err := writeConfig(path, lines); err != nil {
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return fmt.Errorf("codexconfig: 删除备份 %s 失败: %w", backupPath, err)
	}
	return nil
}

// resolveConfigPaths 返回 codex 主目录与 config.toml 绝对路径。
func resolveConfigPaths() (home, configPath string, err error) {
	home, err = FindCodexHome()
	if err != nil {
		return "", "", err
	}
	return home, filepath.Join(home, "config.toml"), nil
}

func modelsCatalogPath(home string) string {
	return filepath.Join(home, modelsFileName)
}

func (m *Manager) currentBaseURL() string {
	if m.baseURL == nil {
		return ""
	}
	return m.baseURL()
}

// isEnabled 判断给定文件内容是否已指向本网关，且 model_catalog_json
// 指向当前 models.json 路径。
func isEnabled(raw []byte, base, catalogPath string) bool {
	if base == "" {
		return false
	}
	lines := strings.Split(string(raw), "\n")
	value, ok := topLevelKey(lines, "model_provider")
	if !ok || value != providerID {
		return false
	}
	blockBase, ok := tableValue(lines, providerHeader, "base_url")
	if !ok || blockBase != base {
		return false
	}
	catalog, ok := topLevelKey(lines, "model_catalog_json")
	return ok && catalog == catalogPath
}

// providerBlock 返回完整的 provider 表块（含嵌套 auth 表）。
func providerBlock(base string) []string {
	return []string{
		providerHeader,
		`name = "` + providerName + `"`,
		`base_url = "` + base + `"`,
		`wire_api = "` + wireAPI + `"`,
		"",
		"[model_providers." + providerID + ".auth]",
		`command = "` + authCommand + `"`,
	}
}

// writeBackup 把当前 model_provider 原值写入侧车文件（0600）。
func writeBackup(path string, lines []string) error {
	value, ok := topLevelKey(lines, "model_provider")
	var state backupState
	if ok {
		state.ModelProvider = &value
	}
	if catalog, ok := topLevelKey(lines, "model_catalog_json"); ok {
		state.ModelCatalogJSON = &catalog
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("codexconfig: 序列化备份失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("codexconfig: 写入备份 %s 失败: %w", path, err)
	}
	return nil
}

// writeConfig 原子写回 config.toml：临时文件 + rename，保留原文件权限。
func writeConfig(path string, lines []string) error {
	return writeBytes(path, []byte(strings.Join(lines, "\n")+"\n"))
}

// writeBytes 原子写文件：临时文件 + rename，保留原文件权限；新文件 0600。
func writeBytes(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.toml.tmp-*")
	if err != nil {
		return fmt.Errorf("codexconfig: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codexconfig: 设置临时文件权限失败: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codexconfig: 写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("codexconfig: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("codexconfig: 替换 %s 失败: %w", path, err)
	}
	return nil
}
