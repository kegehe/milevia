/**
 * 备案号是否该显示。判据抽成纯函数是为了能逐格测：
 * 只有"真正的浏览器"（网页版）要显示；桌面端 Tauri 外壳与 Capacitor 原生包都不是网站。
 *
 * 注意不能只看 isWeb()：getPlatform() 只区分"有没有桌面运行时"，Android 原生包里同样返回
 * "web"，只判它会把备案号带进 App。
 */
export function shouldShowIcpFiling(env: { desktop: boolean; native: boolean }): boolean {
  return !env.desktop && !env.native;
}
