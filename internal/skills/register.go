package skills

import "aistudio/internal/skill"

// RegisterAll 注册全部制作技能
func RegisterAll(reg *skill.Registry) {
	reg.Register(scriptSkill{})
	reg.Register(charactersSkill{})
	// 素材库阶段：分镜设计之前产出一致性素材（角色参考帧 + 场景图）——
	// 生产顺序：场景描述 → 素材 → 分镜 → 视频 → 配音
	reg.Register(assetsSkill{})
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
