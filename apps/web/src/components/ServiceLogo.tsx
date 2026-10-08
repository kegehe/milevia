// MCP 服务的官方品牌图标。
//
// 资产来源（抓取于 2026-09-24，文件名 mcp-*.svg，文件头/`<title>` 都保留官方标识）：
//   github / notion / linear / sentry / jira / stripe —— simple-icons（cdn.simpleicons.org，
//     内嵌官方品牌色：GitHub #181717、Notion #000000、Linear #5E6AD2、Sentry #362D59、
//     Jira #0052CC、Stripe #635BFF）
//   slack —— simple-icons 单色原件，按官方茄紫色 #4A154B 上色
//   playwright —— simple-icons（v9，官方单色原件），按官方绿 #2EAD33 上色
//   context7 —— context7.com 官方站点的 logo 原件（横向 lockup，裁出 28×28 徽标位）
//
// 用 <img> 而不是内联 JSX：与 `AgentLogo` 同一理由 —— 官方 SVG 的渐变/clipPath 固定 ID
// 内联多实例会串；原样引用保证"屏幕上的就是官方那张"。哪个服务用哪个图标的判据在
// `features/mcp/mcp-model.ts` 的 `serviceLogoKey`（按预设名白名单，认不出回落示意图标）。

import context7Logo from "../assets/mcp-context7.svg";
import githubLogo from "../assets/mcp-github.svg";
import jiraLogo from "../assets/mcp-jira.svg";
import linearLogo from "../assets/mcp-linear.svg";
import notionLogo from "../assets/mcp-notion.svg";
import playwrightLogo from "../assets/mcp-playwright.svg";
import sentryLogo from "../assets/mcp-sentry.svg";
import slackLogo from "../assets/mcp-slack.svg";
import stripeLogo from "../assets/mcp-stripe.svg";
import { serviceLogoKey, type ServiceLogoKey } from "../features/mcp/mcp-model";
import type { ReactNode } from "react";

const LOGOS: Readonly<Record<ServiceLogoKey, string>> = {
  github: githubLogo,
  notion: notionLogo,
  linear: linearLogo,
  sentry: sentryLogo,
  slack: slackLogo,
  jira: jiraLogo,
  stripe: stripeLogo,
  playwright: playwrightLogo,
  context7: context7Logo,
};

/** 白名单内渲染官方图标；白名单外渲染调用方给的回落（页面原有的示意图标）。 */
export function ServiceLogo({ service, fallback, size = 16 }: { service: string; fallback: ReactNode; size?: number }) {
  const key = serviceLogoKey(service);
  if (!key) return <>{fallback}</>;
  return <img src={LOGOS[key]} alt="" width={size} height={size} draggable={false} />;
}
