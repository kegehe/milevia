import { Capacitor } from "@capacitor/core";
import { isDesktop } from "../lib/runtime";
import { shouldShowIcpFiling } from "../lib/icp-filing";
import { ExternalLink } from "./ExternalLink";

/** 备案号（工信部要求网页底部标注并链接到备案系统）。只网页端显示，判据见 lib/icp-filing。 */
export const ICP_FILING_NUMBER = "蜀ICP备2025140858号-2";

export function IcpFooter({ className = "" }: { className?: string }) {
  const visible = shouldShowIcpFiling({ desktop: isDesktop(), native: Capacitor.isNativePlatform() });
  if (!visible) return null;
  return (
    <p className={`icp-filing${className ? ` ${className}` : ""}`}>
      <ExternalLink href="https://beian.miit.gov.cn/" target="_blank" rel="noopener noreferrer">
        {ICP_FILING_NUMBER}
      </ExternalLink>
    </p>
  );
}
