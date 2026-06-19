/** @type {import('tailwindcss').Config} */
export default {
  content: ["./src/**/*.{astro,html,js,jsx,md,mdx,ts,tsx}"],
  darkMode: "class",
  theme: {
    extend: {
      colors: {
        // Nexus brand palette — cool slate + violet accent (Llm-gateway aesthetic)
        brand: {
          50: "#f4f4ff",
          100: "#e9e9ff",
          200: "#d0d0ff",
          300: "#a8a8ff",
          400: "#7a7aff",
          500: "#5252ff",
          600: "#3636e6",
          700: "#2828b3",
          800: "#1f1f80",
          900: "#17174d",
        },
        ink: {
          50: "#f7f7fa",
          100: "#eceef4",
          200: "#d3d7e3",
          300: "#a8aec6",
          400: "#6c7395",
          500: "#454c70",
          600: "#2f3656",
          700: "#1f253f",
          800: "#13182c",
          900: "#0a0d1a",
        },
      },
      fontFamily: {
        sans: [
          "Inter",
          "ui-sans-serif",
          "system-ui",
          "-apple-system",
          "BlinkMacSystemFont",
          "Segoe UI",
          "sans-serif",
        ],
        mono: [
          "JetBrains Mono",
          "ui-monospace",
          "SFMono-Regular",
          "Menlo",
          "Monaco",
          "Consolas",
          "monospace",
        ],
      },
      backgroundImage: {
        "hero-grid":
          "linear-gradient(to right, rgba(255,255,255,0.06) 1px, transparent 1px), linear-gradient(to bottom, rgba(255,255,255,0.06) 1px, transparent 1px)",
        "hero-glow":
          "radial-gradient(ellipse 80% 50% at 50% -20%, rgba(82,82,255,0.25), transparent)",
        "card-glow":
          "radial-gradient(ellipse 60% 50% at 50% 0%, rgba(82,82,255,0.18), transparent)",
      },
      backgroundSize: {
        "grid-32": "32px 32px",
      },
      keyframes: {
        "fade-up": {
          "0%": { opacity: "0", transform: "translateY(8px)" },
          "100%": { opacity: "1", transform: "translateY(0)" },
        },
        "pulse-soft": {
          "0%, 100%": { opacity: "0.7" },
          "50%": { opacity: "1" },
        },
      },
      animation: {
        "fade-up": "fade-up 0.5s ease-out forwards",
        "pulse-soft": "pulse-soft 2.4s ease-in-out infinite",
      },
    },
  },
  plugins: [],
};
