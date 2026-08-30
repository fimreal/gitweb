// 主题唯一入口。localStorage('theme') 三态：auto（跟随系统，键不存在）/ light / dark。
// 必须在 <head> 内同步加载：data-theme 先于首帧写入，避免深浅色闪烁。
(function () {
    var KEY = 'theme';
    var ORDER = ['auto', 'light', 'dark'];
    var mq = window.matchMedia('(prefers-color-scheme: dark)');

    var ICONS = {
        auto: '<rect x="2" y="3" width="20" height="14" rx="2"/><path d="M8 21h8M12 17v4"/>',
        light: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41"/>',
        dark: '<path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/>'
    };

    var TITLES = {
        auto: { zh: '主题：跟随系统', en: 'Theme: follow system' },
        light: { zh: '主题：浅色', en: 'Theme: light' },
        dark: { zh: '主题：深色', en: 'Theme: dark' }
    };

    function lang() { return localStorage.getItem('lang') === 'zh' ? 'zh' : 'en'; }
    function mode() { var m = localStorage.getItem(KEY); return ORDER.indexOf(m) > 0 ? m : 'auto'; }
    function resolved() { var m = mode(); return m === 'auto' ? (mq.matches ? 'dark' : 'light') : m; }

    function apply() {
        document.documentElement.setAttribute('data-theme', resolved());
        document.dispatchEvent(new CustomEvent('gitthemechange'));
    }

    function set(next) {
        if (ORDER.indexOf(next) < 0) return;
        if (next === 'auto') localStorage.removeItem(KEY);
        else localStorage.setItem(KEY, next);
        apply();
    }

    function cycle() {
        set(ORDER[(ORDER.indexOf(mode()) + 1) % ORDER.length]);
    }

    // 通用切换按钮：标记 data-theme-toggle，由本文件负责图标、标题与点击。
    function sync() {
        var m = mode(), l = lang(), title = TITLES[m][l];
        if (m === 'auto') {
            title += l === 'zh' ? '（当前 ' + (resolved() === 'dark' ? '深色' : '浅色') + '）'
                                : ' (currently ' + resolved() + ')';
        }
        var svg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"' +
            ' stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + ICONS[m] + '</svg>';
        document.querySelectorAll('[data-theme-toggle]').forEach(function (btn) {
            btn.innerHTML = svg;
            btn.title = title;
            btn.setAttribute('aria-label', title);
            btn.setAttribute('data-mode', m);
        });
    }

    document.addEventListener('gitthemechange', sync);
    mq.addEventListener('change', function () { if (mode() === 'auto') apply(); });

    document.addEventListener('DOMContentLoaded', function () {
        document.querySelectorAll('[data-theme-toggle]').forEach(function (btn) {
            btn.addEventListener('click', cycle);
        });
        sync();
    });

    apply();

    window.GitTheme = { mode: mode, resolved: resolved, set: set, cycle: cycle, sync: sync };
})();
