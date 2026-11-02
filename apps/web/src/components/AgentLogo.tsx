// CLI 工具的官方产品图标。
//
// 资产来源（原样引用、未做 JSX 改写；qoder/codebuddy 的出处另写在各自 SVG 文件头）：
//   claude     —— Anthropic Claude 官方星芒（simple-icons「claude」，官方色 #D97757 内嵌；2026-09-24）
//   openai     —— OpenAI 官方扭结（simple-icons「openai」，官方单色黑；2026-09-24）
//   codebuddy  —— 腾讯 CodeBuddy 官方标志（www.codebuddy.ai 站点 favicon，腾讯云 COS 原件；2026-09-24）
//   qoder      —— Qoder 官方标志徽标（qoder.com 首页 <link rel="icon"> 声明的官方 SVG，阿里云 COS 原件；2026-10-28）
//   opencode   —— OpenCode 官方方环标（simple-icons「opencode」，官方单色黑；2026-10-28）
//   cursor     —— Cursor 官方立方标（simple-icons「cursor」，官方单色黑；2026-10-28）
//
// 用 <img> 而不是内联 JSX：官方 SVG 带渐变 / 滤镜 / clipPath 的固定 ID，内联多实例会
// 相互串 ID；原样引用保证"屏幕上的就是官方那张"。哪个工具用哪个图标的判据在
// `lib/cli-tools-view.ts` 的 `agentLogoKey`（按工具 ID 白名单，认不出回落两字母牌）。

import claudeLogo from "../assets/agent-claude.svg";
import codebuddyLogo from "../assets/agent-codebuddy.svg";
import cursorLogo from "../assets/agent-cursor.svg";
import opencodeLogo from "../assets/agent-opencode.svg";
import openaiLogo from "../assets/agent-openai.svg";
import qoderLogo from "../assets/agent-qoder.svg";
import type { AgentLogoKey } from "../lib/cli-tools-view";

const LOGOS: Readonly<Record<AgentLogoKey, string>> = {
  claude: claudeLogo,
  openai: openaiLogo,
  codebuddy: codebuddyLogo,
  qoder: qoderLogo,
  opencode: opencodeLogo,
  cursor: cursorLogo,
};

/** 自带底色的**徽标类**图标（应用图标形态）：界面里要铺满整个牌位，
 *  牌子自己的底色/边框让位 —— 与"裸图形居中留白"的 glyph 类（Claude/OpenAI/
 *  OpenCode/Cursor）不同。 */
const BADGE_LOGOS: Readonly<Record<AgentLogoKey, boolean>> = {
  claude: false,
  openai: false,
  codebuddy: true,
  qoder: true,
  opencode: false,
  cursor: false,
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
