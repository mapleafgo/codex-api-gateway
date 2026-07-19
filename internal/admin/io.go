package admin

import "os"

// writeFile 是 os.WriteFile 的薄封装，方便测试注入。
func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
