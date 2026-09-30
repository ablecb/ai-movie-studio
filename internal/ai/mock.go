package ai

import "strings"

// MockRespond 离线演示模式：按调用方名称返回确定性的结构化输出。
// 生产技能在 mock 模式下大多自带内置演示数据，这里兜底保证引擎链路可完整演练。
func MockRespond(callName string) string {
	switch {
	case strings.Contains(callName, "script"):
		return `{"title":"归途","logline":"深空第七年，星际信使收到来自地球麦田的信号，决定返航。","genre":"科幻/剧情","style":"胶片质感","themes":["归乡"],"scenes":[{"id":"s1","location":"空间站","time_of_day":"夜","action":"收到信号","dialogue":[{"character":"林","line":"是麦田？"}]}]}`
	case strings.Contains(callName, "characters"):
		return `{"characters":[{"name":"信使·林","appearance":"35岁亚洲女性，短发，左眉浅疤","wardrobe":"深灰航天服","voice":"低沉平静","consistency_tokens":"lin-female35"}]}`
	case strings.Contains(callName, "storyboard"):
		return `{"shots":[{"id":"sh001","scene_id":"s1","description":"特写：信号灯映在眼中","shot_size":"特写","camera_move":"缓推","duration_sec":4,"location":"空间站","characters":["信使·林"]}],"total_duration_sec":4}`
	case strings.Contains(callName, "prompts"):
		return `{"shots":[{"id":"sh001","image_prompt":"cinematic close-up, console lights reflected in eyes, film grain","negative_prompt":"低分辨率, 多余手指, 水印","video_prompt":"缓慢推近","character_refs":["lin-female35"]}]}`
	case strings.Contains(callName, "voiceover"):
		return `{"narration":[{"shot_id":"sh001","speaker":"narrator","text":"一束信号撕开了寂静。","emotion":"悬念"}],"tts_notes":"低音男声"}`
	case strings.Contains(callName, "music"):
		return `{"cue_sheet":[{"id":"cue01","section":"开场","mood":"悬疑","tempo":"60 BPM","instruments":"弦乐","duration_sec":4}],"sound_fx":[{"shot_id":"sh001","name":"蜂鸣","description":"电子蜂鸣"}]}`
	case strings.Contains(callName, "review"):
		return `{"score":8.8,"pass":true,"strengths":["结构完整"],"issues":[],"feedback":"放行"}`
	case strings.Contains(callName, "selfheal"):
		return `{"action":"retry","hint":"只输出合法 JSON，禁止代码围栏与解释文字","root_cause":"输出格式不合规"}`
	default:
		return `{"note":"mock 兜底输出"}`
	}
}
