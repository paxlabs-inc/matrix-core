export default {
  // Primary header navigation
  primary: [
    { label: "Features", url: "/features/" },
    { label: "Architecture", url: "/architecture/" },
    { label: "Use Cases", url: "/use-cases/" },
    { label: "Developers", url: "/developers/", badge: "New" },
    { label: "Enterprise", url: "/enterprise/" },
    { label: "Blog", url: "/blog/" }
  ],

  // CTA buttons in header
  headerCta: [
    { label: "GitHub", url: "https://github.com/paxlabs-inc/matrix-core", external: true, variant: "ghost" },
    { label: "Get Started", url: "/developers/", variant: "primary" }
  ],

  // Mobile-specific groupings
  mobile: {
    main: [
      { label: "Features", url: "/features/" },
      { label: "Architecture", url: "/architecture/" },
      { label: "Use Cases", url: "/use-cases/" },
      { label: "Developers", url: "/developers/" },
      { label: "Enterprise", url: "/enterprise/" },
      { label: "Pricing", url: "/pricing/" },
      { label: "Blog", url: "/blog/" }
    ],
    secondary: [
      { label: "About", url: "/about/" },
      { label: "Press", url: "/press/" },
      { label: "Contact", url: "/contact/" },
      { label: "Docs", url: "https://docs.matrixmcl.com", external: true }
    ]
  }
};
