/* ============================================================
   i18n.js — Internationalization support.
   Fetches translations from API and applies them to the DOM.
   ============================================================ */
'use strict';
import { apiUrl } from './api.js';

export const i18n = {
    currentLang: 'en',
    translations: {},
    englishTranslations: {},
    supportedLangs: ['ar', 'bn', 'cs', 'de', 'en', 'es', 'fr', 'he', 'hi', 'id', 'it', 'ja', 'ko', 'ms', 'nl', 'pl', 'pt', 'ro', 'ru', 'sv', 'th', 'tr', 'uk', 'ur', 'vi', 'zh'],

    async init() {
        // Fetch server config first for language settings
        try {
            const res = await fetch(apiUrl('/api/config'));
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

    // The translation endpoints can be protected together with the dashboard.
    // Retry after a successful login without wiring the language dropdown a
    // second time, and honor the now-accessible server language policy.
    async refreshAfterAuth(config = {}) {
        if (config.lang) this.serverConfig = config.lang;
        const lang = this.detectLanguage();
        await this.loadTranslations(lang);
        this.applyTranslations();
        document.dispatchEvent(new Event('kula-i18n-changed'));
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
            const response = await fetch(apiUrl(`/api/i18n?lang=${lang}`));
            if (!response.ok) throw new Error('Failed to load translations');
            const translations = await response.json();
            if (lang === 'en') {
                this.englishTranslations = translations;
            } else if (Object.keys(this.englishTranslations).length === 0) {
                // The browser endpoint serves one raw locale, while the Go
                // translator has English fallback semantics. Mirror those
                // semantics in the SPA so newly introduced controls remain
                // readable until every locale catches up.
                try {
                    const fallback = await fetch(apiUrl('/api/i18n?lang=en'));
                    if (fallback.ok) this.englishTranslations = await fallback.json();
                } catch (_error) {
                    // A missing fallback must not discard a valid locale.
                }
            }
            this.translations = { ...this.englishTranslations, ...translations };
            this.currentLang = lang;
            localStorage.setItem('kula_lang', lang);
            document.documentElement.lang = lang;

            // Set direction for right-to-left locales.
            document.documentElement.dir = ['ar', 'he', 'ur'].includes(lang) ? 'rtl' : 'ltr';

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

        // Translate accessible names independently from visible labels.
        document.querySelectorAll('[data-i18n-aria-label]').forEach(el => {
            const key = el.getAttribute('data-i18n-aria-label');
            if (this.translations[key]) {
                el.setAttribute('aria-label', this.translations[key]);
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
