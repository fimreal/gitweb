// gitweb toast —— 左下角动态浮窗提示，统一替代 alert/console.error/内联错误。
// 用法：toast.error('加载失败'), toast.success('已刷新'), toast.info('提示')
(function () {
    let container = null;

    function ensureContainer() {
        if (container && document.body.contains(container)) return container;
        container = document.createElement('div');
        container.className = 'toast-container';
        container.setAttribute('aria-live', 'polite');
        document.body.appendChild(container);
        return container;
    }

    function show(message, type, opts) {
        opts = opts || {};
        const duration = opts.duration || (type === 'error' ? 5000 : 2600);
        const c = ensureContainer();

        const item = document.createElement('div');
        item.className = 'toast-item toast-' + (type || 'info');
        item.setAttribute('role', type === 'error' ? 'alert' : 'status');

        const icon = document.createElement('span');
        icon.className = 'toast-icon';
        icon.innerHTML = iconSVG(type);
        const text = document.createElement('span');
        text.className = 'toast-text';
        text.textContent = String(message);
        const close = document.createElement('button');
        close.className = 'toast-close';
        close.setAttribute('aria-label', '关闭');
        close.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>';

        item.appendChild(icon);
        item.appendChild(text);
        item.appendChild(close);
        c.appendChild(item);

        // 入场：下一帧加 visible 触发过渡
        requestAnimationFrame(() => requestAnimationFrame(() => item.classList.add('visible')));

        let timer = null;
        function dismiss() {
            if (timer) { clearTimeout(timer); timer = null; }
            item.classList.remove('visible');
            item.addEventListener('transitionend', () => item.remove(), { once: true });
            // 兜底
            setTimeout(() => { if (item.parentNode) item.remove(); }, 400);
        }
        timer = setTimeout(dismiss, duration);
        close.addEventListener('click', dismiss);
        item.addEventListener('mouseenter', () => { if (timer) { clearTimeout(timer); timer = null; } });
        item.addEventListener('mouseleave', () => { timer = setTimeout(dismiss, duration); });

        return item;
    }

    function iconSVG(type) {
        if (type === 'error') return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8v5"/><circle cx="12" cy="16.5" r="0.6" fill="currentColor"/></svg>';
        if (type === 'success') return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M8.5 12.5l2.5 2.5 4.5-5"/></svg>';
        return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 11v5"/><circle cx="12" cy="7.5" r="0.6" fill="currentColor"/></svg>';
    }

    window.toast = {
        error: (m, o) => show(m, 'error', o),
        success: (m, o) => show(m, 'success', o),
        info: (m, o) => show(m, 'info', o),
        show
    };
})();
