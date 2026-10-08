package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// 多启动命令（见 docs/12 §8）。
//
// 一个项目往往不止一种启动方式：`npm run dev` / `npm run dev:test` / `docker compose up`
// 参数不同就是不同环境。原先 project_run_configs 只存一条 command，这里把它扩成
// commands 列表 + selected_command_id，启动时选一条执行。
//
// 三条约定：
//  1. 落地只有一份真源：commands。RunConfig.Command 退化为"当前选中命令的文本镜像"，
//     只是为了兼容旧库、以及在 sqlite 里直接看库的人；读回来会被重新同步，不参与判定。
//  2. 旧数据零迁移成本：commands 为空而 command 有值时，读取阶段就地迁移成一条命令
//     （ID 固定为 legacyRunCommandID），老项目升级后不会丢命令。
//  3. 选中项永远有效：normalize 之后 selectedCommandId 必定指向 commands 里存在的条目，
//     否则回落到第一条；命令列表为空时为空串。
const (
	// legacyRunCommandID 是旧单命令配置迁移出来的那条命令的 ID，稳定不变。
	legacyRunCommandID = "legacy-default"
	// adHocRunCommandID 是"用请求体里的一次性命令启动"（不落库）时使用的 ID。
	adHocRunCommandID = "ad-hoc"
)

// RunCommand 是一条具名启动命令。
type RunCommand struct {
	// ID 标识这一条，选择、去重、前端列表 key 都以它为准。由前端生成，后端兜底补齐。
	ID string `json:"id"`
	// Name 是展示名（如"开发环境"）；留空时前端回落到命令文本。
	Name string `json:"name"`
	// Command 是实际执行的命令行。
	Command string `json:"command"`
	// EnvVars 是这条命令专属的环境变量，启动时叠加在 RunConfig.EnvVars 之上（同名覆盖）。
	// 用于"同一个入口、不同环境参数"的场景；留空表示只用全局环境变量。
	EnvVars map[string]string `json:"envVars,omitempty"`
}

// normalizeRunCommands 把配置收敛成可持久化、可下发的规范形态：迁移旧单命令、丢弃
// 前端留下的空行、补齐并去重 ID、修正选中项、同步 Command 镜像。幂等 —— 对已规范的
// 配置再调用一次不会有任何变化。
func normalizeRunCommands(c *RunConfig) {
	// 旧配置（只有单条 command）迁移成列表里的一条。用户清空所有命令时前端会把
	// command 一并置空，所以这里不会把"已删除的命令"复活。
	if len(c.Commands) == 0 && strings.TrimSpace(c.Command) != "" {
		c.Commands = []RunCommand{{ID: legacyRunCommandID, Command: strings.TrimSpace(c.Command)}}
	}

	commands := make([]RunCommand, 0, len(c.Commands))
	used := make(map[string]struct{}, len(c.Commands))
	for _, command := range c.Commands {
		command.ID = strings.TrimSpace(command.ID)
		command.Name = strings.TrimSpace(command.Name)
		command.Command = strings.TrimSpace(command.Command)
		// 一个字都没填的行（前端"添加一条"点出来但没写）不落库。
		// 只填了名称没填命令的行会留下，由校验收尾并给出明确报错。
		if command.Name == "" && command.Command == "" && len(command.EnvVars) == 0 {
			continue
		}
		if command.ID == "" {
			command.ID = uuid.NewString()
		}
		for {
			if _, ok := used[command.ID]; !ok {
				break
			}
			command.ID = uuid.NewString()
		}
		used[command.ID] = struct{}{}
		if len(command.EnvVars) == 0 {
			command.EnvVars = nil
		}
		commands = append(commands, command)
	}
	c.Commands = commands

	c.SelectedCommandID = strings.TrimSpace(c.SelectedCommandID)
	if _, ok := c.commandByID(c.SelectedCommandID); !ok {
		c.SelectedCommandID = ""
		if len(commands) > 0 {
			c.SelectedCommandID = commands[0].ID
		}
	}
	if selected, ok := c.commandByID(c.SelectedCommandID); ok {
		c.Command = selected.Command
	} else {
		c.Command = ""
	}
}

// commandByID 按 ID 查一条命令；空 ID 恒视为不存在。
func (c RunConfig) commandByID(id string) (RunCommand, bool) {
	if id == "" {
		return RunCommand{}, false
	}
	for _, command := range c.Commands {
		if command.ID == id {
			return command, true
		}
	}
	return RunCommand{}, false
}

// resolveRunCommand 决定本次启动实际执行哪条命令：显式指定（请求体里带的选择）优先，
// 其次配置里持久化的选中项，最后回落到第一条。
//
// 显式指定的 ID 对不上时报错，而不是悄悄换一条：想启动"生产环境"却跑成"开发环境"，
// 比启动失败更糟。
func (c RunConfig) resolveRunCommand(id string) (RunCommand, error) {
	if len(c.Commands) == 0 {
		return RunCommand{}, errors.New("请先配置启动命令")
	}
	if id != "" {
		command, ok := c.commandByID(id)
		if !ok {
			return RunCommand{}, errors.New("选中的启动命令已不存在，请刷新页面后重新选择")
		}
		return command, nil
	}
	if command, ok := c.commandByID(c.SelectedCommandID); ok {
		return command, nil
	}
	return c.Commands[0], nil
}

// mergedRunEnvVars 叠加全局环境变量与命令专属环境变量（同名以后者为准）。返回值恒非
// nil，可直接交给 runner 使用。
func mergedRunEnvVars(global map[string]string, command RunCommand) map[string]string {
	merged := make(map[string]string, len(global)+len(command.EnvVars))
	for key, value := range global {
		merged[key] = value
	}
	for key, value := range command.EnvVars {
		merged[key] = value
	}
	return merged
}

// validateRunCommands 逐条校验启动命令：命令文本非空、专属环境变量合法。不要求
// "至少一条" —— 保存阶段允许先把命令清空，真正启动时由 resolveRunCommand 兜底。
func validateRunCommands(commands []RunCommand) error {
	for index, command := range commands {
		if command.Command == "" {
			return fmt.Errorf("第 %d 条启动命令不能为空", index+1)
		}
		if err := validateRunEnvironmentVariables(command.EnvVars); err != nil {
			return fmt.Errorf("启动命令「%s」的环境变量无效：%w", runCommandDisplayName(command, index), err)
		}
	}
	return nil
}

// runCommandDisplayName 是错误文案里指代某条命令的称呼：优先名字，其次序号。
func runCommandDisplayName(command RunCommand, index int) string {
	if command.Name != "" {
		return command.Name
	}
	return fmt.Sprintf("第 %d 条", index+1)
}
