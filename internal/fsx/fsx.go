// Package fsx 提供文件与媒体探测的公共工具。
// 核心是原子写：本项目的状态文件（state.json）与渲染产物（png/mp4/srt）
// 都是"已付费资产"，进程中断留下的半截文件会导致断点续跑跳过坏文件、
// 或直接丢掉整次运行状态，因此所有落盘必须先写临时文件再原子改名。
package fsx

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// AtomicWrite 原子写文件：写同目录临时文件后 rename 覆盖目标。
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 失败路径清理；成功后文件已不存在，Remove 报错可忽略
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// FileExists 文件存在且非空
func FileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

// AtomicWriteJSON 原子写入 JSON（缩进美化，便于人工复核与 diff）。
func AtomicWriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(path, data, 0o644)
}

// ReadJSON 读取并解析 JSON 文件。
func ReadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
