// CLI 工具的官方产品图标。
//
// 资产来源（抓取于 2026-09-24，文件头都写有出处，原样引用、未做 JSX 改写）：
//   claude     —— Anthropic Claude 官方星芒（simple-icons「claude」，官方色 #D97757 内嵌）
//   openai     —— OpenAI 官方扭结（simple-icons「openai」，官方单色黑）
//   codebuddy  —— 腾讯 CodeBuddy 官方标志（www.codebuddy.ai 站点 favicon，腾讯云 COS 原件）
//
// 用 <img> 而不是内联 JSX：官方 SVG 带渐变 / 滤镜 / clipPath 的固定 ID，内联多实例会
// 相互串 ID；原样引用保证"屏幕上的就是官方那张"。哪个工具用哪个图标的判据在
// `lib/cli-tools-view.ts` 的 `agentLogoKey`（按工具 ID 白名单，认不出回落两字母牌）。

import claudeLogo from "../assets/agent-claude.svg";
import codebuddyLogo from "../assets/agent-codebuddy.svg";
import openaiLogo from "../assets/agent-openai.svg";
import type { AgentLogoKey } from "../lib/cli-tools-view";

const LOGOS: Readonly<Record<AgentLogoKey, string>> = {
  claude: claudeLogo,
  openai: openaiLogo,
  codebuddy: codebuddyLogo,
};

/** 自带底色的**徽标类**图标（应用图标形态）：界面里要铺满整个牌位，
 *  牌子自己的底色/边框让位 —— 与"裸图形居中留白"的 glyph 类（Claude/OpenAI）不同。 */
const BADGE_LOGOS: Readonly<Record<AgentLogoKey, boolean>> = {
  claude: false,
  openai: false,
  codebuddy: true,
};

export function AgentLogo({ logo, size = 18 }: { logo: AgentLogoKey; size?: number }) {
  const badge = BADGE_LOGOS[logo];
  return <img
    src={LOGOS[logo]}
    alt=""
    width={size}
    height={size}
    draggable={false}
    {...(badge ? { "data-logo-badge": "" } : {})}
  />;
}
