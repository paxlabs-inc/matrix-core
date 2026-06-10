Paxeer — Legal & Compliance (static HTML)
=========================================

Contents
  index.html ................ legal hub linking all 14 documents
  *.html .................... the 14 converted legal documents
  css/paxeer-legal.css ...... standalone stylesheet (Paxeer Brand v3.0 tokens)
  fonts/ .................... drop the Paxeer Grand Sans OTF/WOFF2 files here

Notes
  - Fully static. Open index.html in a browser or drop the folder on any host
    (Vercel, S3, nginx). No build step.
  - Inter + JetBrains Mono load from Google Fonts. The brand display face
    (Paxeer Grand Sans) is referenced at /fonts/PPPangramSansRounded-*.otf and
    falls back to Inter until those files are present.
  - The original uploaded CSS was a Tailwind v4 source file (@import
    'tailwindcss', @theme, @apply). It can't render standalone, so this
    stylesheet reproduces the same v3.0 design tokens as plain CSS. To use it
    inside the Tailwind/Next.js app instead, keep the markup and class names and
    map them onto the existing token variables.
