/* ============================================================
   i18n.js — Internationalization support.
   Fetches translations from API and applies them to the DOM.
   ============================================================ */
'use strict';
export const i18n = {
    currentLang: 'en',
    translations: {},
    supportedLangs: ['ar', 'bn', 'cs', 'de', 'en', 'es', 'fr', 'he', 'hi', 'id', 'it', 'ja', 'ko', 'ms', 'nl', 'pl', 'pt', 'ro', 'ru', 'sv', 'th', 'tr', 'uk', 'ur', 'vi', 'zh'],

    async init() {
        // Fetch server config first for language settings
        try {
            const res = await fetch('/api/config');
            if (res.ok) {
                const config = await res.json();
                this.serverConfig = config.lang || { default: 'en', force: false };
            }
        } catch (e) {
            console.error('Failed to fetch lang config:', e);
            this.serverConfig = { default: 'en', force: false };
        }

        this.currentLang = this.detectLanguage();
        await this.loadTranslations(this.currentLang);
        this.applyTranslations();
        this.setupDropdown();
    },

    detectLanguage() {
        const defaultLang = (this.serverConfig && this.serverConfig.default) || 'en';

        // 0. If forced by config
        if (this.serverConfig && this.serverConfig.force) {
            return defaultLang;
        }

        // 1. Check local storage
        const saved = localStorage.getItem('kula_lang');
        if (saved && this.supportedLangs.includes(saved)) return saved;

        // 2. Check browser language
        const browserLang = (navigator.language || navigator.userLanguage || 'en').split('-')[0].toLowerCase();
        if (this.supportedLangs.includes(browserLang)) return browserLang;

        // 3. Fallback
        return defaultLang;
    },

    async loadTranslations(lang) {
        try {
            const response = await fetch(`/api/i18n?lang=${lang}`);
            if (!response.ok) throw new Error('Failed to load translations');
            this.translations = await response.json();
            this.currentLang = lang;
            localStorage.setItem('kula_lang', lang);
            document.documentElement.lang = lang;

            // Set direction for Arabic
            document.documentElement.dir = (lang === 'ar') ? 'rtl' : 'ltr';

            // Highlight active language in dropdown
            this.updateActiveHighlight();
            this.updateLangCodeDisplay();
        } catch (error) {
            console.error('i18n error:', error);
            const defaultLang = (this.serverConfig && this.serverConfig.default) || 'en';
            // Fallback to configured default if not already trying it
            if (lang !== defaultLang) {
                await this.loadTranslations(defaultLang);
            } else if (lang !== 'en') {
                // Absolute fallback to English
                await this.loadTranslations('en');
            }
        }
    },

    updateActiveHighlight() {
        const options = document.querySelectorAll('.lang-option');
        options.forEach(opt => {
            if (opt.getAttribute('data-lang') === this.currentLang) {
                opt.classList.add('active');
            } else {
                opt.classList.remove('active');
            }
        });
    },

    updateLangCodeDisplay() {
        const el = document.getElementById('active-lang-code');
        if (el) {
            el.textContent = this.currentLang.toUpperCase();
        }
    },

    applyTranslations() {
        // Translate textContent
        document.querySelectorAll('[data-i18n]').forEach(el => {
            const key = el.getAttribute('data-i18n');
            if (this.translations[key]) {
                el.textContent = this.translations[key];
            }
        });

        // Translate placeholders
        document.querySelectorAll('[data-i18n-placeholder]').forEach(el => {
            const key = el.getAttribute('data-i18n-placeholder');
            if (this.translations[key]) {
                el.placeholder = this.translations[key];
            }
        });

        // Translate titles (tooltips)
        document.querySelectorAll('[data-i18n-title]').forEach(el => {
            const key = el.getAttribute('data-i18n-title');
            if (this.translations[key]) {
                el.title = this.translations[key];
            }
        });
    },

    t(key) {
        return this.translations[key] || key;
    },

    setupDropdown() {
        const btn = document.getElementById('lang-btn');
        const menu = document.getElementById('lang-menu');

        if (!btn || !menu) return;

        btn.addEventListener('click', (e) => {
            e.stopPropagation();
            menu.classList.toggle('hidden');
        });

        document.addEventListener('click', (e) => {
            if (!menu.contains(e.target) && e.target !== btn) {
                menu.classList.add('hidden');
            }
        });

        const options = document.querySelectorAll('.lang-option');
        options.forEach(opt => {
            opt.addEventListener('click', async () => {
                const lang = opt.getAttribute('data-lang');
                menu.classList.add('hidden');
                if (lang === this.currentLang) return;

                await this.loadTranslations(lang);
                this.applyTranslations();

                document.dispatchEvent(new Event('kula-i18n-changed'));
            });
        });
    }
};
