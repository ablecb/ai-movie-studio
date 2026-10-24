package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"aistudio/internal/promspec"
	"aistudio/internal/shotkit"
)

// 参考帧提示词预览："编辑提示词重出"弹窗的数据源。
// 与 CLI 素材命令共用同一套解析（shotkit.ResolveStyle / RefPromptFor），
// 保证弹窗里展示的提示词 = 实际会发送的提示词。

// handleRefPrompt GET /api/runs/{id}/ref-prompt?char=角色名
// 返回该角色每一帧（base + 变体）当前实际会发送的提示词，供人工编辑。
func (s *Server) handleRefPrompt(w http.ResponseWriter, r *http.Request) {
	run := r.PathValue("id")
	char := strings.TrimSpace(r.URL.Query().Get("char"))
	if char == "" {
		writeJSON(w, 400, map[string]any{"error": "缺少 char 参数"})
		return
	}
	runDir := filepath.Join(s.root, "runs", run)
	style, err := shotkit.ResolveStyle(runDir)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}

	// 角色规格：shotspec 优先（字段更结构化），角色圣经兜底——与素材命令一致
	var chars []promspec.CharacterSpec
	if data, rerr := os.ReadFile(filepath.Join(runDir, "artifacts", "shotspec.json")); rerr == nil {
		var kit promspec.Kit
		if json.Unmarshal(data, &kit) == nil && len(kit.Characters) > 0 {
			chars = kit.Characters
		}
	}
	if len(chars) == 0 {
		if data, rerr := os.ReadFile(filepath.Join(runDir, "artifacts", "characters.json")); rerr == nil {
			chars, _ = shotkit.CharactersFromBible(data)
		}
	}
	var target *promspec.CharacterSpec
	for i := range chars {
		if chars[i].Name == char {
			target = &chars[i]
			break
		}
	}
	if target == nil {
		writeJSON(w, 404, map[string]any{"error": "角色不在角色圣经中: " + char})
		return
	}

	// 帧清单：持久化的素材规划（含变体与上次人工编辑），没有则 base 单帧
	frames := shotkit.LoadAssetPlan(runDir).FramesFor(char)
	out := make([]map[string]any, 0, len(frames))
	for _, fr := range frames {
		out = append(out, map[string]any{
			"variant": fr.Variant,
			"purpose": fr.Purpose,
			"prompt":  shotkit.RefPromptFor(style, *target, fr),
		})
	}
	writeJSON(w, 200, map[string]any{"char": char, "frames": out})
}
