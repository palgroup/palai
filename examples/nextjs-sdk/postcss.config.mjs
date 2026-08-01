// Tailwind v4 needs only this one PostCSS plugin; there is no tailwind.config.js in v4 — the theme
// lives in CSS (app/globals.css @theme). Added 2026-08-01 for AI Elements, which is shadcn-based and
// therefore Tailwind-only. NOTE: apps/web-console deliberately uses plain CSS and is untouched by this.
const config = { plugins: { "@tailwindcss/postcss": {} } };
export default config;
