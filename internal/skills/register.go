package skills

import "aistudio/internal/skill"

// RegisterAll 注册全部制作技能
func RegisterAll(reg *skill.Registry) {
	reg.Register(scriptSkill{})
	reg.Register(charactersSkill{})
	reg.Register(storyboardSkill{})
	reg.Register(promptsSkill{})
	reg.Register(voiceoverSkill{})
	reg.Register(musicSkill{})
	// shotspec 是"生成前"的关键一步：把台词绑定到镜头、把摄影语言编译成提示词
	reg.Register(shotSpecSkill{})
	reg.Register(subtitlesSkill{})
	reg.Register(editplanSkill{})
	reg.Register(reviewSkill{})
}
