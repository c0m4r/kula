/* ============================================================
   Kula Landing Page - Interactive behaviors
   ============================================================ */

document.addEventListener('DOMContentLoaded', () => {

    let currentTranslations = {};

    // ---- Theme Toggle Logic ----
    const themeBtn = document.getElementById('btn-theme');
    const previewDark = document.getElementById('preview-dark');
    const previewLight = document.getElementById('preview-light');

    // Check for saved theme preference or default to dark
    const savedTheme = localStorage.getItem('kula-theme') || 'dark';
    if (savedTheme === 'light') {
        document.body.classList.add('light-mode');
        showLightPreview();
    }

    function showLightPreview() {
        if (previewDark && previewLight) {
            previewDark.classList.add('hidden');
            previewLight.classList.remove('hidden');
        }
    }

    function showDarkPreview() {
        if (previewDark && previewLight) {
            previewLight.classList.add('hidden');
            previewDark.classList.remove('hidden');
        }
    }

    themeBtn.addEventListener('click', () => {
        document.body.classList.toggle('light-mode');
        const isLight = document.body.classList.contains('light-mode');
        localStorage.setItem('kula-theme', isLight ? 'light' : 'dark');

        if (isLight) {
            showLightPreview();
        } else {
            showDarkPreview();
        }
    });

    // ---- Fetch GitHub Stars ----
    async function fetchStars() {
        const badges = document.querySelectorAll('.github-stars-count');
        const DEFAULT_STARS = 1324;
        const CACHE_KEY = 'kula-stars';
        const TIMESTAMP_KEY = 'kula-stars-time';
        const SIX_HOURS = 6 * 60 * 60 * 1000;

        const cachedStars = localStorage.getItem(CACHE_KEY);
        const cachedTime = localStorage.getItem(TIMESTAMP_KEY);
        const now = Date.now();

        const updateUI = (count) => {
            const starsText = count >= 1000 ? (count / 1000).toFixed(1) + 'k' : count;
            badges.forEach(badge => {
                badge.textContent = '⭐ ' + starsText;
                badge.classList.remove('hidden');
            });
        };

        // 1. Check if cached and valid (less than 1 hour old)
        if (cachedStars && cachedTime && (now - parseInt(cachedTime)) < SIX_HOURS) {
            updateUI(parseInt(cachedStars));
            return;
        }

        // 2. Try to fetch from API
        try {
            const resp = await fetch('https://api.github.com/repos/c0m4r/kula');
            if (resp.ok) {
                const data = await resp.json();
                const stars = data.stargazers_count;
                if (stars !== undefined) {
                    localStorage.setItem(CACHE_KEY, stars);
                    localStorage.setItem(TIMESTAMP_KEY, now.toString());
                    updateUI(stars);
                    return;
                }
            } else if (resp.status === 403) {
                console.warn('GitHub API rate limit exceeded. Using default stars.');
            }
        } catch (e) {
            console.error('Failed to fetch stars:', e);
        }

        // 3. Fallback to default value if API fails or rate limited
        updateUI(DEFAULT_STARS);
    }
    fetchStars();

    // ---- Install tabs ----
    document.querySelectorAll('.install-tab').forEach(tab => {
        tab.addEventListener('click', () => {
            document.querySelectorAll('.install-tab').forEach(t => t.classList.remove('active'));
            document.querySelectorAll('.install-panel').forEach(p => p.classList.remove('active'));

            tab.classList.add('active');
            const panel = document.getElementById('panel-' + tab.dataset.tab);
            if (panel) panel.classList.add('active');
        });
    });

    // ---- Copy logic ----
    function showCopied(btn) {
        const originalText = btn.textContent;
        btn.textContent = currentTranslations['btn_done'] || 'done';
        btn.style.color = 'var(--accent-green)';
        btn.style.borderColor = 'var(--accent-green)';
        setTimeout(() => {
            btn.textContent = originalText;
            btn.style.color = '';
            btn.style.borderColor = '';
        }, 2000);
    }

    function copyToClipboard(text, btn) {
        if (navigator.clipboard && navigator.clipboard.writeText) {
            navigator.clipboard.writeText(text).then(() => showCopied(btn)).catch(() => fallbackCopy(text, btn));
        } else {
            fallbackCopy(text, btn);
        }
    }

    function fallbackCopy(text, btn) {
        const textarea = document.createElement('textarea');
        textarea.value = text;
        textarea.style.position = 'fixed';
        textarea.style.left = '-9999px';
        textarea.style.top = '0';
        document.body.appendChild(textarea);
        textarea.focus();
        textarea.select();
        try {
            document.execCommand('copy');
            showCopied(btn);
        } catch (err) {
            console.error('Unable to copy', err);
        }
        document.body.removeChild(textarea);
    }

    // ---- Copy buttons ----
    document.querySelectorAll('.copy-btn').forEach(btn => {
        btn.addEventListener('click', () => {
            let codeElement;
            if (btn.id === 'copy-install-btn') {
                codeElement = document.getElementById('install-command');
            } else {
                codeElement = btn.closest('pre').querySelector('code');
            }

            if (!codeElement) return;
            copyToClipboard(codeElement.textContent.trim(), btn);
        });
    });


    // ---- Install Command Logic ----
    const installCommand = document.getElementById('install-command');
    const toolTabs = document.querySelectorAll('.tool-tab');
    const verifyCheckbox = document.getElementById('verify-checksum');

    let selectedTool = 'curl';

    const updateInstallCommand = () => {
        const isVerified = verifyCheckbox.checked;
        let command = '';

        if (isVerified) {
            if (selectedTool === 'curl') {
                command = `KULA_INSTALL=$(mktemp) ; curl -o \${KULA_INSTALL} -fsSL https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install_v2.sh ; echo "bad61ee9eed4595d20fa7e613bd27c3b8700c67f8a5fcac756d282a811705398  \${KULA_INSTALL}" | sha256sum -c || rm -f \${KULA_INSTALL} ; bash \${KULA_INSTALL} ; rm -f \${KULA_INSTALL}`;
            } else {
                command = `KULA_INSTALL=$(mktemp) ; wget -O \${KULA_INSTALL} -q https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install_v2.sh ; echo "bad61ee9eed4595d20fa7e613bd27c3b8700c67f8a5fcac756d282a811705398  \${KULA_INSTALL}" | sha256sum -c || rm -f \${KULA_INSTALL} ; bash \${KULA_INSTALL} ; rm -f \${KULA_INSTALL}`;
            }
        } else {
            if (selectedTool === 'curl') {
                command = `bash -c "$(curl -fsSL https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install_v2.sh)"`;
            } else {
                command = `bash -c "$(wget -qO- https://raw.githubusercontent.com/c0m4r/kula/refs/heads/main/addons/install_v2.sh)"`;
            }
        }

        installCommand.textContent = command;
    };

    toolTabs.forEach(tab => {
        tab.addEventListener('click', () => {
            toolTabs.forEach(t => t.classList.remove('active'));
            tab.classList.add('active');
            selectedTool = tab.dataset.tool;
            updateInstallCommand();
        });
    });

    if (verifyCheckbox) {
        verifyCheckbox.addEventListener('change', updateInstallCommand);
    }


    // ---- Scroll-reveal (fade-up) ----
    const observer = new IntersectionObserver((entries) => {
        entries.forEach(entry => {
            if (entry.isIntersecting) {
                entry.target.classList.add('visible');
                observer.unobserve(entry.target);
            }
        });
    }, { threshold: 0.1 });

    document.querySelectorAll('.fade-up').forEach(el => observer.observe(el));

    // ---- Navigation background transparent to blur on scroll ----
    window.addEventListener('scroll', () => {
        const nav = document.getElementById('nav');
        if (window.scrollY > 40) {
            nav.style.boxShadow = '0 4px 30px rgba(0, 0, 0, 0.1)';
        } else {
            nav.style.boxShadow = 'none';
        }
    }, { passive: true });

    // ---- i18n Logic ----
    const langSelect = document.getElementById('lang-select');

    async function loadLocale(lang) {
        try {
            const response = await fetch(`locales/${lang}.json`);
            if (!response.ok) throw new Error(`Could not load ${lang} locale`);
            currentTranslations = await response.json();
            applyTranslations();
            document.documentElement.lang = lang;
            localStorage.setItem('kula-lang', lang);
            langSelect.value = lang;
        } catch (error) {
            console.error('Error loading locale:', error);
            if (lang !== 'en') loadLocale('en');
        }
    }

    function sanitizeHTML(html) {
        const parser = new DOMParser();
        const doc = parser.parseFromString(html, 'text/html');
        const allowedTags = ['A', 'BR', 'STRONG', 'B', 'I', 'EM', 'U', 'SUP', 'SUB'];
        const allowedAttrs = {
            'A': ['href', 'rel', 'target', 'title']
        };

        function clean(node) {
            const children = Array.from(node.childNodes);
            for (const child of children) {
                if (child.nodeType === Node.ELEMENT_NODE) {
                    const tag = child.tagName.toUpperCase();
                    if (!allowedTags.includes(tag)) {
                        // Replace unauthorized tag with its text content
                        const text = document.createTextNode(child.textContent);
                        node.replaceChild(text, child);
                    } else {
                        // Filter attributes
                        const attrs = Array.from(child.attributes);
                        const validAttrs = allowedAttrs[tag] || [];
                        for (const attr of attrs) {
                            if (!validAttrs.includes(attr.name.toLowerCase())) {
                                child.removeAttribute(attr.name);
                            }
                        }

                        if (tag === 'A') {
                            const href = child.getAttribute('href');
                            if (href) {
                                try {
                                    const url = new URL(href, document.baseURI);
                                    if (url.protocol !== 'https:' && url.protocol !== 'http:' && url.protocol !== 'mailto:') {
                                        child.removeAttribute('href');
                                    }
                                } catch {
                                    child.removeAttribute('href'); // malformed URL — strip it
                                }
                            }
                            // Enforce security attributes for target="_blank"
                            if (child.getAttribute('target') === '_blank') {
                                child.setAttribute('rel', 'noopener noreferrer');
                            }
                        }
                        clean(child);
                    }
                }
            }
        }

        clean(doc.body);
        return doc.body.innerHTML;
    }

    function applyTranslations() {
        // Text translations
        document.querySelectorAll('[data-i18n]').forEach(el => {
            const key = el.getAttribute('data-i18n');
            if (currentTranslations[key]) {
                el.textContent = currentTranslations[key];
                // If it's the title tag, also update document.title
                if (el.tagName === 'TITLE') {
                    document.title = currentTranslations[key];
                }
            }
        });

        // HTML translations (for tags like <br> or <a>)
        document.querySelectorAll('[data-i18n-html]').forEach(el => {
            const key = el.getAttribute('data-i18n-html');
            if (currentTranslations[key]) {
                el.innerHTML = sanitizeHTML(currentTranslations[key]);
            }
        });

        // Meta description update
        document.querySelectorAll('[data-i18n-meta]').forEach(el => {
            const key = el.getAttribute('data-i18n-meta');
            if (currentTranslations[key]) {
                el.setAttribute('content', currentTranslations[key]);
            }
        });
    }

    langSelect.addEventListener('change', (e) => {
        loadLocale(e.target.value);
    });

    // Detect language: URL (?lang=) -> Saved -> Browser -> Default (en)
    const urlParams = new URLSearchParams(window.location.search);
    const urlLang = urlParams.get('lang');
    const savedLang = localStorage.getItem('kula-lang');
    const browserLang = navigator.language.split('-')[0];
    const supportedLangs = ['en', 'ar', 'bn', 'cs', 'de', 'es', 'fr', 'he', 'hi', 'id', 'it', 'ja', 'ko', 'ms', 'nl', 'pl', 'pt', 'ro', 'ru', 'sv', 'th', 'tr', 'uk', 'ur', 'vi', 'zh'];

    let initialLang = 'en';

    if (urlLang && supportedLangs.includes(urlLang)) {
        initialLang = urlLang;
    } else if (savedLang && supportedLangs.includes(savedLang)) {
        initialLang = savedLang;
    } else if (supportedLangs.includes(browserLang)) {
        initialLang = browserLang;
    }

    loadLocale(initialLang);

    // ---- Smooth scroll for anchor links ----
    document.querySelectorAll('a[href^="#"]').forEach(a => {
        a.addEventListener('click', e => {
            const href = a.getAttribute('href');
            if (href === '#') return;
            const target = document.querySelector(href);
            if (target) {
                e.preventDefault();
                const navHeight = document.getElementById('nav').offsetHeight;
                window.scrollTo({
                    top: target.offsetTop - navHeight - 20,
                    behavior: 'smooth'
                });
            }
        });
    });
});
