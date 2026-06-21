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

        const authType = document.getElementById('auth_type');
        const tokenInput = document.getElementById('token');
        const usernameInput = document.getElementById('username');
        const passwordInput = document.getElementById('password');

        if (authType) {
            authType.addEventListener('change', () => {
                tokenInput.style.display = 'none';
                usernameInput.style.display = 'none';
                passwordInput.style.display = 'none';

                if (authType.value === 'token') {
                    tokenInput.style.display = 'block';
                } else if (authType.value === 'basic') {
                    usernameInput.style.display = 'block';
                    passwordInput.style.display = 'block';
                }
            });
        }

        form.addEventListener('submit', async (e) => {
            e.preventDefault();

            const formData = new FormData(form);
            const pathidInput = formData.get('pathid');
            const payload = {
                git_url: formData.get('git_url'),
                pathid: pathidInput && pathidInput.trim() !== '' ? pathidInput.trim() : undefined,
                ref: formData.get('ref') || 'main'
            };

            const authType = formData.get('auth_type');
            if (authType === 'token') {
                payload.auth = {
                    type: 'token',
                    token: formData.get('token')
                };
            } else if (authType === 'basic') {
                payload.auth = {
                    type: 'basic',
                    username: formData.get('username'),
                    password: formData.get('password')
                };
            }

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
                } else {
                    resultDiv.className = 'error';
                    resultDiv.innerHTML = `<strong>${currentLang === 'zh' ? '✗ 错误' : '✗ Error'}:</strong> ${result.error}`;
                }

                resultDiv.style.display = 'block';
            } catch (error) {
                const resultDiv = document.getElementById('result');
                resultDiv.className = 'error';
                resultDiv.innerHTML = `<strong>${currentLang === 'zh' ? '✗ 网络错误' : '✗ Network error'}:</strong> ${error.message}`;
                resultDiv.style.display = 'block';
            }
        });
    }

    async function loadSites() {
        try {
            const response = await fetch('/api/sites');
            if (!response.ok) return;

            const data = await response.json();
            if (!data.sites || data.sites.length === 0) return;

            const sitesGrid = document.getElementById('sites-list');
            sitesGrid.innerHTML = data.sites.map(site => `
                <div class="site-card">
                    <a href="/${site.pathid}/" target="_blank" style="font-weight: bold; color: inherit; text-decoration: none;">
                        ${site.pathid}
                    </a>
                    <a href="/${site.pathid}/" target="_blank">${site.git_url}</a>
                    <div class="ref">${site.ref}</div>
                </div>
            `).join('');

            sitesGrid.style.display = 'grid';
        } catch (error) {
            console.error('Failed to load sites:', error);
        }
    }

    document.addEventListener('DOMContentLoaded', () => {
        initTheme();
        initLang();
        initForm();
        if (document.getElementById('sites-list')) {
            loadSites();
        }
    });
})();
