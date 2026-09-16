package info.keyanjia.milevia;

import android.os.Bundle;
import android.view.animation.LinearInterpolator;
import androidx.core.splashscreen.SplashScreen;
import com.getcapacitor.BridgeActivity;

/**
 * Capacitor 的宿主 Activity，只接管「启动页淡出」与「WebView 底色」两件启动期的事。
 *
 * 为什么不用 @capacitor/splash-screen 插件：它的 showOnLaunch() 调完
 * installSplashScreen() 之后会挂一个 OnPreDrawListener 持续返回 false 来阻止首屏绘制，
 * 直到 launchShowDuration 走完（默认 500ms）。也就是说它会【强制启动页多停留一段固定
 * 时间】，启动反而变慢；而我们只需要一次淡出。androidx.core:core-splashscreen 本来就在
 * app/build.gradle 的依赖里，直接用它即可，不引入额外插件依赖。
 *
 * installSplashScreen() 必须在 super.onCreate() 之前调用，否则在 Android 12+ 上不生效。
 * 它会读取主题里的 postSplashScreenTheme 来还原 Activity 主题 —— styles.xml 里已把它
 * 指向 AppTheme.NoActionBar，与 BridgeActivity.onCreate 自己设的完全一致，不会打架。
 *
 * Android 11 及以下由 androidx 的兼容实现接管，读的仍是
 * windowSplashScreenBackground / windowSplashScreenAnimatedIcon 两个属性，
 * 两个版本因此看到同一个品牌启动页。但低版本不实现 setOnExitAnimationListener，
 * 所以低版本没有淡出、是直接切换 —— 两端底色相同，看不出接缝。
 */
public class MainActivity extends BridgeActivity {

    /** 启动页淡出时长。 */
    private static final long SPLASH_FADE_OUT_MS = 220L;

    @Override
    public void onCreate(Bundle savedInstanceState) {
        SplashScreen splashScreen = SplashScreen.installSplashScreen(this);
        super.onCreate(savedInstanceState);

        applyWebViewBackground();

        splashScreen.setOnExitAnimationListener(splashScreenView ->
            splashScreenView
                .getView()
                .animate()
                .alpha(0f)
                .setInterpolator(new LinearInterpolator())
                .setDuration(SPLASH_FADE_OUT_MS)
                .withEndAction(splashScreenView::remove)
                .start()
        );
    }

    /**
     * 把 WebView 的底色对齐启动页与首屏，避免启动页淡出时闪一下白。
     *
     * WebView 在解析到 body 的背景之前用的是系统默认白，而启动页背景与首屏
     * .mobile-remote 都是 #f3f8f4，两者相差可见 —— 不设这一项，启动页淡出的瞬间
     * 会露出白底。Capacitor 本身支持用 capacitor.config.ts 的 android.backgroundColor
     * 配这一项（Bridge.java 里读取并调用 setBackgroundColor），但那条路径要等 cap sync
     * 才会进包；写在这里能立刻生效，也和启动页的其它处理放在同一处。
     *
     * 必须在 super.onCreate() 之后调用：WebView 是 BridgeActivity.load() 里创建 Bridge
     * 时才有的。WebView 创建失败时 bridge 为 null（BridgeActivity 会切到 no_webview
     * 布局），那种情况直接跳过。
     */
    private void applyWebViewBackground() {
        if (bridge == null || bridge.getWebView() == null) {
            return;
        }
        bridge.getWebView().setBackgroundColor(getColor(R.color.launch_background));
    }
}
