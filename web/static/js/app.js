(function() {
    let currentLang = localStorage.getItem('lang') || 
                      (navigator.language.startsWith('zh') ? 'zh' : 'en');

    function initTheme() {
        // 自动检测系统暗黑模式
        const savedTheme = localStorage.getItem('theme');
        const systemDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
        const initialTheme = savedTheme || (systemDark ? 'dark' : 'light');
        
        document.documentElement.setAttribute('data-theme', initialTheme);

        const themeToggle = document.getElementById('theme-toggle');
        if (themeToggle) {
            themeToggle.addEventListener('click', () => {
                const current = document.documentElement.getAttribute('data-theme');
                const newTheme = current === 'dark' ? 'light' : 'dark';
                document.documentElement.setAttribute('data-theme', newTheme);
                localStorage.setItem('theme', newTheme);
            });
        }
        
        // 监听系统暗黑模式变化
        window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', (e) => {
            if (!localStorage.getItem('theme')) {
                document.documentElement.setAttribute('data-theme', e.matches ? 'dark' : 'light');
            }
        });
    }

    function initLang() {
        applyLang(currentLang);

        const langToggle = document.getElementById('lang-toggle');
        if (langToggle) {
            langToggle.textContent = currentLang === 'en' ? '中文' : 'EN';
            langToggle.addEventListener('click', () => {
                currentLang = currentLang === 'en' ? 'zh' : 'en';
                localStorage.setItem('lang', currentLang);
                langToggle.textContent = currentLang === 'en' ? '中文' : 'EN';
                applyLang(currentLang);
            });
        }
    }

    function applyLang(lang) {
        document.querySelectorAll('[data-en][data-zh]').forEach(el => {
            const text = lang === 'zh' ? el.getAttribute('data-zh') : el.getAttribute('data-en');
            if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') {
                el.placeholder = text;
            } else {
                el.textContent = text;
            }
        });

        // title 提示翻译（如高级选项 summary 只剩图标，文字走 title）
        document.querySelectorAll('[data-en-title][data-zh-title]').forEach(el => {
            el.title = lang === 'zh' ? el.getAttribute('data-zh-title') : el.getAttribute('data-en-title');
        });

        const mainInput = document.getElementById('git_url');
        if (mainInput) {
            const placeholder = lang === 'zh' ?
                mainInput.getAttribute('data-placeholder-zh') :
                mainInput.getAttribute('data-placeholder-en');
            mainInput.placeholder = placeholder;
        }
    }

    function initForm() {
        const form = document.getElementById('register-form');
        if (!form) return;

        form.addEventListener('submit', async (e) => {
            e.preventDefault();

            const formData = new FormData(form);
            const pathidInput = formData.get('pathid');
            // 复选框勾选 = 公开显示（hidden=false）；未勾选 = 隐藏（hidden=true）
            const showInList = document.getElementById('hidden-toggle').checked;
            const payload = {
                git_url: formData.get('git_url'),
                pathid: pathidInput && pathidInput.trim() !== '' ? pathidInput.trim() : undefined,
                ref: formData.get('ref') || 'main',
                hidden: !showInList
            };

            try {
                const response = await fetch('/api/sites', {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json',
                    },
                    body: JSON.stringify(payload)
                });

                const result = await response.json();
                const resultDiv = document.getElementById('result');

                if (response.ok) {
                    resultDiv.className = 'success';
                    resultDiv.innerHTML = `
                        <p><strong>${currentLang === 'zh' ? '✓ 注册成功！' : '✓ Registration successful!'}</strong></p>
                        <p><strong>Path ID:</strong> <a href="/${result.pathid}/" target="_blank">${result.pathid}</a></p>
                        <p><strong>URL:</strong> <a href="${result.url}" target="_blank">${result.url}</a></p>
                    `;
                    document.getElementById('git_url').value = '';
                    document.getElementById('pathid').value = '';
                    loadSites();
                    toast.success(currentLang === 'zh' ? '注册成功' : 'Registration successful');
                } else {
                    resultDiv.className = 'error';
                    resultDiv.innerHTML = `<strong>${currentLang === 'zh' ? '✗ 错误' : '✗ Error'}:</strong> ${result.error}`;
                    toast.error((currentLang === 'zh' ? '注册失败' : 'Registration failed') + ': ' + (result.error || response.status));
                }

                resultDiv.style.display = 'block';
            } catch (error) {
                const resultDiv = document.getElementById('result');
                resultDiv.className = 'error';
                resultDiv.innerHTML = `<strong>${currentLang === 'zh' ? '✗ 网络错误' : '✗ Network error'}:</strong> ${error.message}`;
                resultDiv.style.display = 'block';
                toast.error((currentLang === 'zh' ? '网络错误' : 'Network error') + ': ' + error.message);
            }
        });
    }

    async function loadSites() {
        try {
            const response = await fetch('/api/sites');
            if (!response.ok) return;

            const data = await response.json();
            if (!data.sites || data.sites.length === 0) return;

            // 首页内联只展示最近 3 个公开站点（按创建时间降序），
            // 完整列表走右下角浮层。
            const recent = data.sites
                .slice()
                .sort((a, b) => (b.created_at || '').localeCompare(a.created_at || ''))
                .slice(0, 3);

            const sitesGrid = document.getElementById('sites-list');
            const sitesTitle = document.getElementById('sites-title');
            sitesGrid.innerHTML = recent.map(site => `
                <a class="site-card" href="/${site.pathid}/" target="_blank" rel="noopener">
                    <div class="site-card-head">
                        <span class="pathid">${site.pathid}</span>
                        <svg class="open-arrow" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M7 17L17 7"/><path d="M8 7h9v9"/></svg>
                    </div>
                    <span class="git-url">${site.git_url}</span>
                    <span class="ref-chip">
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="6" cy="6" r="2.5"/><circle cx="6" cy="18" r="2.5"/><circle cx="18" cy="6" r="2.5"/><path d="M6 8.5v7"/><path d="M6 12h6a3 3 0 0 0 3-3V8.5"/></svg>
                        ${site.ref}
                    </span>
                </a>
            `).join('');

            sitesGrid.style.display = 'grid';
            if (sitesTitle) sitesTitle.style.display = 'block';
        } catch (error) {
            console.error('Failed to load sites:', error);
        }
    }

    // =========================================================
    //  站点列表浮层：入口按钮 → 打开面板 → 搜索 + 翻页
    // =========================================================
    (function initSitesOverlay() {
        const entryBtn = document.getElementById('sites-entry-btn');
        const overlay = document.getElementById('sites-overlay');
        const backdrop = document.getElementById('sites-overlay-backdrop');
        const closeBtn = document.getElementById('sites-panel-close');
        const filterInp = document.getElementById('sites-filter');
        const listEl = document.getElementById('sites-panel-list');
        const emptyEl = document.getElementById('sites-panel-empty');
        const pagerEl = document.getElementById('sites-panel-pager');
        const prevBtn = document.getElementById('pager-prev');
        const nextBtn = document.getElementById('pager-next');
        const infoEl = document.getElementById('pager-info');
        if (!entryBtn || !overlay) return;

        const PER_PAGE = 5;
        let allSites = [];
        let filtered = [];
        let page = 0, totalPages = 0;

        function escapeHtml(s) {
            var d = document.createElement('div');
            d.textContent = s == null ? '' : String(s);
            return d.innerHTML;
        }

        function siteCard(s) {
            return '<a class="site-card" href="/' + escapeHtml(s.pathid) + '/" target="_blank" rel="noopener">'
                + '<div class="site-card-head"><span class="pathid">' + escapeHtml(s.pathid) + '</span>'
                + '<svg class="open-arrow" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M7 17L17 7"/><path d="M8 7h9v9"/></svg></div>'
                + '<span class="git-url">' + escapeHtml(s.git_url) + '</span>'
                + '<span class="ref-chip"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="6" cy="6" r="2.5"/><circle cx="6" cy="18" r="2.5"/><circle cx="18" cy="6" r="2.5"/><path d="M6 8.5v7"/><path d="M6 12h6a3 3 0 0 0 3-3V8.5"/></svg>'
                + escapeHtml(s.ref) + '</span></a>';
        }

        function filter() {
            var q = (filterInp.value || '').trim().toLowerCase();
            filtered = q ? allSites.filter(function(s) {
                return (s.pathid||'').toLowerCase().indexOf(q)>=0 ||
                    (s.git_url||'').toLowerCase().indexOf(q)>=0 ||
                    (s.ref||'').toLowerCase().indexOf(q)>=0 ||
                    (s.provider||'').toLowerCase().indexOf(q)>=0;
            }) : allSites;
            page = 0;
            renderPage();
        }

        function renderPage() {
            totalPages = Math.max(1, Math.ceil(filtered.length / PER_PAGE));
            if (page >= totalPages) page = totalPages - 1;
            var start = page * PER_PAGE;
            var chunk = filtered.slice(start, start + PER_PAGE);
            if (chunk.length === 0 && filtered.length === 0) {
                listEl.innerHTML = '';
                listEl.style.display = 'none';
                emptyEl.style.display = 'block';
                pagerEl.style.display = 'none';
            } else {
                emptyEl.style.display = 'none';
                listEl.style.display = 'block';
                listEl.innerHTML = '<div class="sites-grid">' + chunk.map(siteCard).join('') + '</div>';
                prevBtn.disabled = page <= 0;
                nextBtn.disabled = page >= totalPages - 1;
                infoEl.textContent = (page+1) + ' / ' + totalPages;
                pagerEl.style.display = 'flex';
            }
        }

        prevBtn.addEventListener('click', function(){ if(page>0){page--;renderPage();} });
        nextBtn.addEventListener('click', function(){ if(page<totalPages-1){page++;renderPage();} });
        filterInp.addEventListener('input', filter);

        async function open() {
            overlay.style.display = 'flex';
            try {
                var resp = await fetch('/api/sites');
                if (!resp.ok) return;
                var data = await resp.json();
                allSites = data.sites || [];
            } catch(e) { console.error('sites overlay:', e); }
            filter();
        }

        function close() { overlay.style.display = 'none'; }
        entryBtn.addEventListener('click', open);
        backdrop.addEventListener('click', close);
        closeBtn.addEventListener('click', close);
    })();

    // =========================================================
    //  底部应用简介：箭头点击展开/收起
    // =========================================================
    (function initAbout() {
        const toggle = document.getElementById('about-toggle');
        const panel = document.getElementById('about-panel');
        if (!toggle || !panel) return;

        toggle.addEventListener('click', () => {
            const open = toggle.getAttribute('aria-expanded') === 'true';
            if (open) {
                panel.hidden = true;
                toggle.setAttribute('aria-expanded', 'false');
            } else {
                panel.hidden = false;
                toggle.setAttribute('aria-expanded', 'true');
            }
        });
    })();

    document.addEventListener('DOMContentLoaded', () => {
        initTheme();
        initLang();
        initForm();
        if (document.getElementById('sites-list')) {
            loadSites();
        }
    });
})();
