# Implementation Plan: Matrix Marketing Website

## Overview

This plan implements the Matrix Marketing Website as a static site using Eleventy (11ty), Tailwind CSS 4, Basecoat CSS components, and vanilla JavaScript. Tasks are ordered: project scaffolding → theme & styling → layout templates → reusable macros → pages → JavaScript interactivity → content collections → SEO/optimization → validation → integration wiring.

## Tasks

- [x] 1. Set up project structure and build configuration
  - [x] 1.1 Initialize project with package.json and install dependencies
    - Create package.json with scripts for dev, build, and test
    - Install @11ty/eleventy, tailwindcss, @tailwindcss/cli, sharp, nunjucks, markdown-it, @11ty/eleventy-plugin-rss, @grimlink/eleventy-plugin-lucide-icons
    - Install devDependencies: vitest, fast-check, html-validate
    - _Requirements: 18.1, 18.2_


  - [x] 1.2 Create Eleventy configuration file (.eleventy.js)
    - Configure input/output directories (docs/src → _site)
    - Register Nunjucks as template engine
    - Register Lucide icons plugin
    - Register RSS plugin
    - Add passthrough copy for static assets
    - Register blog posts and use cases collections
    - _Requirements: 11.5, 16.1, 16.2, 16.5_

  - [x] 1.3 Create Tailwind CSS configuration and Matrix theme CSS
    - Create tailwind.config.js with content paths and custom theme extensions
    - Create src/css/matrix-theme.css with CSS custom properties for dark-mode-first branding
    - Define --background, --foreground, --primary, --card, --muted, --border, --ring variables using oklch
    - Define optional .light class overrides
    - _Requirements: 2.1, 2.2, 2.3, 2.4, 3.1, 3.2, 3.3_

  - [x] 1.4 Create directory structure for source files
    - Create docs/src/_data/, docs/src/_includes/layouts/, docs/src/_includes/macros/, docs/src/_includes/partials/
    - Create docs/src/blog/, docs/src/use-cases/, docs/src/developers/, docs/src/enterprise/
    - Create src/js/, src/css/, src/assets/images/, src/assets/social/
    - Create tests/unit/, tests/integration/
    - _Requirements: 1.1_


- [x] 2. Implement site data layer and configuration
  - [x] 2.1 Create global site configuration (docs/src/_data/site.js)
    - Define site title, tagline, description, URL (env-aware), author, brand tokens
    - Define primary nav array with label/url for Features, Architecture, Use Cases, Developers, Enterprise, Blog
    - Define footerNav with Product, Developers, Company, Legal sections
    - _Requirements: 1.3, 1.5, 4.1_

  - [x] 2.2 Create features data file (docs/src/_data/features.js)
    - Define feature entries with title, description, icon, link, and category fields
    - Categorize into "core", "developer", "enterprise"
    - _Requirements: 1.1, 12.2_

  - [x] 2.3 Create pricing data file (docs/src/_data/pricing.js)
    - Define Open Source, Pro, and Enterprise plans with name, price, description, features array, cta, ctaUrl, highlighted
    - Ensure only Pro plan has highlighted: true
    - _Requirements: 12.5_

  - [x] 2.4 Create navigation data file (docs/src/_data/navigation.js)
    - Export structured nav items for header and footer consumption
    - Include badge support for items (e.g., "New")
    - _Requirements: 1.3, 1.4_


- [x] 3. Implement layout templates
  - [x] 3.1 Create base layout template (docs/src/_includes/layouts/base.njk)
    - Render HTML5 shell with lang, charset, viewport meta
    - Include SEO meta partial in head
    - Inline critical CSS in head
    - Link Tailwind/Basecoat CSS bundle
    - Load deferred JS scripts
    - Render {{ content | safe }} for child templates
    - _Requirements: 11.1, 18.7, 17.1, 17.2, 17.3_

  - [x] 3.2 Create page layout template (docs/src/_includes/layouts/page.njk)
    - Extend base.njk
    - Include header partial and footer partial
    - Wrap content in max-width container with centered layout
    - _Requirements: 11.2, 8.2_

  - [x] 3.3 Create landing layout template (docs/src/_includes/layouts/landing.njk)
    - Extend base.njk
    - Include header partial and footer partial
    - Allow full-width sections without max-width container constraints
    - _Requirements: 11.3, 8.7_

  - [x] 3.4 Create blog-post layout template (docs/src/_includes/layouts/blog-post.njk)
    - Extend page.njk
    - Display article metadata (date, author, tags)
    - Render markdown content with prose styling
    - _Requirements: 11.4_


- [x] 4. Implement global partials (header, footer, SEO meta)
  - [x] 4.1 Create header/navigation partial (docs/src/_includes/partials/header.njk)
    - Render sticky nav with Matrix logo and primary nav links from site data
    - Add data-sticky-nav attribute for JS initialization
    - Include mobile hamburger toggle with data-mobile-toggle and data-mobile-menu attributes
    - Apply aria-expanded and aria-hidden attributes for accessibility
    - Highlight current page link using page.url comparison with accent color
    - _Requirements: 13.1, 13.5, 4.4, 20.4_

  - [x] 4.2 Create footer partial (docs/src/_includes/partials/footer.njk)
    - Render 4-column grouped links: Product, Developers, Company, Legal
    - Include copyright and PaxLabs branding
    - Apply external link attributes (rel, target) for external URLs
    - _Requirements: 1.5, 23.2_

  - [x] 4.3 Create SEO meta partial (docs/src/_includes/partials/seo-meta.njk)
    - Generate title tag from page title + site title
    - Generate meta description from page description
    - Generate Open Graph tags (og:title, og:description, og:image, og:url)
    - Generate Twitter card meta tags
    - _Requirements: 17.1, 17.2, 17.3_


- [x] 5. Implement reusable section macros
  - [x] 5.1 Create hero section macro (docs/src/_includes/macros/hero.njk)
    - Accept title, subtitle, cta_primary, cta_secondary, animation_type parameters
    - Render large bold headline (48px+ desktop, 32px+ mobile) with concise subtitle
    - Render primary CTA (white bg, black text, pill) and secondary CTA (ghost style)
    - Add data-animate attributes for scroll animation initialization
    - _Requirements: 12.1, 8.3, 7.1, 7.2_

  - [x] 5.2 Create feature grid macro (docs/src/_includes/macros/feature-grid.njk)
    - Accept features array, columns count, and style variant
    - Render cards using Basecoat card classes with icon, title, description
    - Apply responsive grid (stacks on mobile)
    - Add data-animate="fade-up" on each card
    - _Requirements: 12.2, 12.7, 8.5_

  - [x] 5.3 Create stats bar macro (docs/src/_includes/macros/stats-bar.njk)
    - Accept array of { value, unit, label } objects
    - Render stat items with data-counter attribute for JS animation
    - Apply horizontal layout on desktop, stacked on mobile
    - _Requirements: 12.3_

  - [x] 5.4 Create code example macro (docs/src/_includes/macros/code-example.njk)
    - Accept title, code, language, description parameters
    - Render styled code block with syntax highlighting classes
    - Add data-typewriter attribute for typewriter animation
    - Apply deep charcoal background (#0D0D0D) and monospace font
    - _Requirements: 12.4, 2.3, 5.2_

  - [x] 5.5 Create pricing table macro (docs/src/_includes/macros/pricing-table.njk)
    - Accept plans array and highlight indicator
    - Render cards for each plan with name, price, features list, CTA button
    - Visually emphasize highlighted plan with accent border/glow
    - _Requirements: 12.5_

  - [x] 5.6 Create CTA banner macro (docs/src/_includes/macros/cta-banner.njk)
    - Accept heading, description, button_text, button_url parameters
    - Render full-width section with centered text and primary CTA button
    - _Requirements: 12.6, 8.7_


- [x] 6. Checkpoint - Verify build pipeline and templates
  - Ensure `npx @11ty/eleventy` builds without errors
  - Verify all layouts, partials, and macros render correctly
  - Ensure all tests pass, ask the user if questions arise.

- [x] 7. Implement site pages
  - [x] 7.1 Create landing page (docs/src/index.njk)
    - Use landing.njk layout
    - Compose with hero, statsBar, featureGrid, codeExample, ctaBanner macros
    - Use consumer-friendly language and outcomes-focused copy
    - _Requirements: 1.2, 9.1, 9.2, 9.5_

  - [x] 7.2 Create features page (docs/src/features.njk)
    - Use page.njk layout
    - Display full feature grid organized by category (core, developer, enterprise)
    - Use alternating left-right layout for feature descriptions
    - _Requirements: 1.1, 8.5, 9.1_

  - [x] 7.3 Create architecture page (docs/src/architecture.njk)
    - Use page.njk layout
    - Include technical diagrams and system component descriptions
    - Use precise technical language for developer audience
    - Add interactive architecture explorer container with data attributes
    - _Requirements: 1.1, 10.2_

  - [x] 7.4 Create use cases listing and entries
    - Create docs/src/use-cases/index.njk with category filtering UI
    - Create docs/src/use-cases/defi-automation.md, enterprise-workflows.md, developer-tools.md
    - Each entry uses page.njk layout with front matter metadata
    - _Requirements: 1.1, 16.5_

  - [x] 7.5 Create developers page (docs/src/developers/index.njk)
    - Use page.njk layout
    - Include code examples, API references, quickstart instructions
    - Use technical language framed in developer experience terms
    - _Requirements: 1.1, 10.2, 10.3, 10.6_

  - [x] 7.6 Create enterprise page (docs/src/enterprise/index.njk)
    - Use page.njk layout
    - Include metrics, compliance references, integration capabilities
    - Use formal corporate language framing features as business impact
    - _Requirements: 1.1, 10.1, 10.4, 10.5_

  - [x] 7.7 Create pricing page (docs/src/pricing.njk)
    - Use page.njk layout
    - Render pricing table macro with data from pricing.js
    - Use consumer-friendly language
    - _Requirements: 1.1, 12.5, 9.1_

  - [x] 7.8 Create blog listing page (docs/src/blog/index.njk)
    - Use page.njk layout
    - List posts from blog collection with pagination
    - Display title, date, excerpt, tags for each post card
    - _Requirements: 1.1, 16.1, 16.2_

  - [x] 7.9 Create sample blog posts (docs/src/blog/*.md)
    - Create 2-3 sample posts with valid front matter (title, date, author, tags, excerpt, image)
    - Use blog-post.njk layout
    - Ensure dates are not in future, tags have at least one entry, excerpts <= 200 chars
    - _Requirements: 16.1, 16.6_

  - [x] 7.10 Create about, press, and contact pages
    - Create docs/src/about.njk, docs/src/press.njk, docs/src/contact.njk
    - Use page.njk layout with appropriate consumer-facing copy
    - _Requirements: 1.1, 1.6, 9.1_


- [x] 8. Implement JavaScript animation and interactivity modules
  - [x] 8.1 Create ScrollAnimator module (src/js/animations.js)
    - Implement IntersectionObserver-based scroll-triggered animation
    - Set initial hidden state (opacity-0, translate-y-4) on observed elements
    - Apply animation class exactly once when element enters viewport then unobserve
    - Respect prefers-reduced-motion media query
    - Gracefully degrade if IntersectionObserver is unsupported
    - _Requirements: 14.1, 14.2, 14.3, 14.4, 14.5, 25.1_

  - [x] 8.2 Create CodeTypewriter module (src/js/code-typewriter.js)
    - Implement character-by-character rendering with configurable speed
    - Pause between lines (default 1000ms)
    - Start animation when element scrolls into view
    - _Requirements: 15.1, 15.2, 15.3_

  - [x] 8.3 Create StatsCounter module (src/js/stats-counter.js)
    - Animate numbers from 0 to target value on scroll into view
    - Support configurable duration and suffix
    - Use requestAnimationFrame for smooth animation
    - _Requirements: 15.4_

  - [x] 8.4 Create StickyNav module (src/js/nav.js)
    - Implement scroll-aware hide/show behavior (hide on scroll down past 100px, show on scroll up)
    - Apply backdrop blur and border when page is scrolled
    - Throttle scroll handler to 16ms (60fps) with passive listener
    - Implement mobile menu toggle with ARIA attribute updates
    - _Requirements: 13.1, 13.2, 13.3, 13.4, 13.5, 13.6_

  - [x] 8.5 Create main.js entry point (src/js/main.js)
    - Import and initialize all modules on DOMContentLoaded
    - Wrap each initialization in try/catch for graceful degradation
    - Log errors to console without breaking page rendering
    - _Requirements: 25.1_

  - [ ]* 8.6 Write unit tests for ScrollAnimator
    - Test that animation class is applied when isIntersecting is true
    - Test that observer.unobserve is called after animation
    - Test initial hidden state is applied to observed elements
    - Test prefers-reduced-motion skips animation setup
    - _Requirements: 14.1, 14.2, 14.3, 14.4_

  - [ ]* 8.7 Write unit tests for StatsCounter
    - Test animation produces expected intermediate and final values
    - Test counter reaches target value at end of duration
    - _Requirements: 15.4_


- [x] 9. Implement custom CSS animations and responsive styles
  - [x] 9.1 Create animation keyframes CSS (src/css/animations.css)
    - Define animate-fade-up, animate-fade-in, animate-slide-right keyframes
    - Use transform and opacity only (GPU-composited properties)
    - Include prefers-reduced-motion override that disables all animations
    - _Requirements: 14.1, 14.4, 20.7_

  - [x] 9.2 Implement responsive utility styles and spacing system
    - Ensure 80px+ vertical padding between major sections
    - Apply 8px spacing scale throughout
    - Enforce max-width 1440px centered content containers
    - Ensure no horizontal overflow at any viewport width (320px–2560px)
    - Set minimum 14px font size on mobile
    - _Requirements: 8.1, 8.2, 8.6, 21.1, 21.2, 21.4_

  - [x] 9.3 Implement typography and component styling
    - Configure primary font (Inter/system sans-serif) and monospace font (Fira Code/JetBrains Mono)
    - Apply font-weight 400 for body, 500-600 for headings
    - Apply tight letter spacing on large headings
    - Style buttons (pill primary, ghost secondary), inputs, tables, badges per spec
    - Define border radius tokens (4-8px cards, pill buttons, 4px badges)
    - _Requirements: 5.1, 5.2, 5.3, 5.4, 5.5, 6.1, 6.2, 6.3, 6.4, 7.1, 7.2, 7.3, 7.4, 7.5_


- [x] 10. Checkpoint - Verify pages render and interactions work
  - Ensure `npx @11ty/eleventy` builds successfully with all pages
  - Verify JS modules initialize without errors
  - Ensure all tests pass, ask the user if questions arise.

- [x] 11. Implement SEO, sitemap, and RSS feed generation
  - [x] 11.1 Create sitemap template (docs/src/sitemap.njk)
    - Generate valid XML sitemap conforming to sitemap.org protocol
    - Include all pages with absolute URLs using production site URL
    - Assign priority 1.0 to landing page
    - Ensure no duplicate URL entries
    - _Requirements: 17.4, 17.5, 17.6_

  - [x] 11.2 Create RSS feed template (docs/src/feed.njk)
    - Generate valid RSS 2.0 XML feed
    - Include at most 20 most recent blog posts
    - Format dates in RFC 822 format
    - Include title, link, description, pubDate, guid for each item
    - _Requirements: 16.3, 16.4_

  - [ ]* 11.3 Write property test for sitemap generation
    - **Property 19: Sitemap Uniqueness and Validity**
    - **Property 20: Sitemap Absolute URLs**
    - Test that for any set of page URLs, generated sitemap has no duplicates and all URLs are absolute
    - **Validates: Requirements 17.4, 17.6**

  - [ ]* 11.4 Write property test for RSS feed
    - **Property 14: RSS Feed Item Limit**
    - **Property 15: RSS Feed Format Compliance**
    - Test that for any N blog posts, feed contains at most 20 items with valid RFC 822 dates
    - **Validates: Requirements 16.3, 16.4**


- [x] 12. Implement data validation system
  - [x] 12.1 Create validation utility module (src/js/validation.js or build-time script)
    - Implement page metadata validation (title 10-70 chars, description 50-160 chars)
    - Implement navigation item validation (label ≤30 chars, URL starts with / or https://)
    - Implement feature entry validation (title ≤50 chars, description ≤200 chars, valid category)
    - Implement pricing plan validation (≥3 features, only one highlighted)
    - Implement blog post validation (date not future, ≥1 tag, excerpt ≤200 chars)
    - Wire validation into Eleventy build via .eleventy.js transforms or data cascade
    - _Requirements: 26.1, 26.2, 26.3, 26.4, 26.5, 16.6_

  - [ ]* 12.2 Write property tests for page metadata validation
    - **Property 17: Page Metadata Validation**
    - Test titles between 10-70 chars accepted, outside rejected
    - Test descriptions between 50-160 chars accepted, outside rejected
    - **Validates: Requirements 17.1, 17.2, 26.1, 26.2**

  - [ ]* 12.3 Write property tests for navigation item validation
    - **Property 34: Navigation Item Validation**
    - Test labels ≤30 chars accepted, longer rejected
    - Test URLs starting with / or https:// accepted, others rejected
    - **Validates: Requirement 26.3**

  - [ ]* 12.4 Write property tests for feature entry validation
    - **Property 35: Feature Entry Validation**
    - Test title ≤50 chars, description ≤200 chars, valid categories accepted
    - Test invalid data rejected
    - **Validates: Requirement 26.4**

  - [ ]* 12.5 Write property tests for pricing plan validation
    - **Property 36: Pricing Plan Validation**
    - Test plans with ≥3 features accepted, fewer rejected
    - Test at most one highlighted plan accepted
    - **Validates: Requirement 26.5**

  - [ ]* 12.6 Write property test for blog post validation
    - **Property 16: Blog Post Validation**
    - Test future dates rejected, past dates accepted
    - Test empty tags rejected, non-empty accepted
    - Test excerpts >200 chars rejected
    - **Validates: Requirement 16.6**


- [ ] 13. Implement asset optimization pipeline
  - [~] 13.1 Create image optimization build step
    - Configure sharp to generate WebP/AVIF variants at 640, 1024, 1440, 1920px widths
    - Generate responsive srcset attributes for image elements
    - Apply content-based hashing to output filenames
    - Generate manifest.json mapping original paths to hashed filenames
    - _Requirements: 19.1, 19.2, 19.4_

  - [~] 13.2 Implement lazy loading and image dimension attributes
    - Add loading="lazy" and decoding="async" to below-fold images
    - Add explicit width and height attributes to all img elements
    - Ensure above-fold hero images load eagerly
    - _Requirements: 19.3, 19.5_

  - [~] 13.3 Implement critical CSS inlining
    - Extract above-the-fold critical CSS
    - Inline it in the HTML <head> of base.njk template
    - Load remaining CSS asynchronously
    - _Requirements: 18.7_

- [ ] 14. Implement accessibility and security features
  - [~] 14.1 Add ARIA attributes to all interactive components
    - Ensure all buttons, menu toggles, dialogs, and tabs have appropriate ARIA attributes
    - Implement keyboard navigation for nav menus, dialogs, and tab components
    - Implement focus trapping in dialogs with return focus on close
    - _Requirements: 20.1, 20.2, 20.3, 20.4_

  - [~] 14.2 Add external link attributes
    - Apply rel="noopener noreferrer" and target="_blank" to all external links (href starting with http)
    - Validate in templates and partials
    - _Requirements: 23.2_

  - [~] 14.3 Add security headers configuration
    - Create _headers file or netlify.toml / vercel.json with security headers
    - Configure HSTS, CSP (same-origin scripts/styles), X-Content-Type-Options, X-Frame-Options, Referrer-Policy
    - Add SRI hashes for any CDN-loaded scripts
    - _Requirements: 22.1, 22.2, 22.3, 22.4, 22.5, 22.6_


- [ ] 15. Implement error handling and graceful degradation
  - [~] 15.1 Implement JavaScript graceful degradation
    - Wrap all JS module initializations in try/catch blocks in main.js
    - Log errors to console without breaking page render
    - Ensure content is visible and accessible when JS fails
    - _Requirements: 25.1_

  - [~] 15.2 Implement font fallback strategy
    - Configure system font stack as fallback for custom fonts
    - Use font-display: swap to prevent FOIT
    - Ensure no layout shift when fonts load or fail
    - _Requirements: 25.2, 5.6_

  - [~] 15.3 Implement build-time error reporting
    - Ensure missing template references cause build failures with descriptive errors
    - Log warnings for missing image assets with alt text fallback rendering
    - _Requirements: 25.3, 25.4_

- [~] 16. Checkpoint - Full integration verification
  - Ensure `npx @11ty/eleventy` builds all pages without errors or warnings
  - Run `npx vitest --run` to verify all unit and property tests pass
  - Verify all internal links resolve to existing output files
  - Ensure all tests pass, ask the user if questions arise.

- [ ] 17. Integration tests and link validation
  - [ ]* 17.1 Write integration test for full build
    - Run Eleventy build and verify exit code 0
    - Assert all expected output files exist in _site/
    - Verify no unrendered {{ }} blocks in output HTML
    - _Requirements: 24.1, 11.5_

  - [ ]* 17.2 Write link integrity test
    - Crawl all internal links in generated HTML
    - Verify each href starting with "/" resolves to an existing file in _site/
    - Verify external links have rel="noopener noreferrer" and target="_blank"
    - _Requirements: 23.1, 23.2_

  - [ ]* 17.3 Write property test for scroll animation behavior
    - **Property 11: Animation Applied Exactly Once**
    - **Property 12: Initial Hidden State on Animated Elements**
    - Test that animation class is applied once and observer disconnects
    - Test initial hidden state is applied before viewport entry
    - **Validates: Requirements 14.2, 14.3**

  - [ ]* 17.4 Write property test for blog post sort order
    - **Property 13: Blog Post Collection Sort Order**
    - Test that for any set of blog posts, the collection returns them in strictly descending date order
    - **Validates: Requirement 16.1**

- [~] 18. Final checkpoint - Complete build and test verification
  - Run full build: `npx @11ty/eleventy`
  - Run all tests: `npx vitest --run`
  - Verify CSS bundle < 50KB gzipped and JS bundle < 20KB gzipped
  - Ensure all tests pass, ask the user if questions arise.

## Notes

- Tasks marked with `*` are optional and can be skipped for faster MVP
- Each task references specific requirements for traceability
- Checkpoints ensure incremental validation at logical milestones
- Property tests validate universal correctness properties from the design document
- Unit tests validate specific examples and edge cases
- The site uses JavaScript (vanilla ES modules), Nunjucks templates, CSS (Tailwind + Basecoat), and Eleventy as the build system
- All CSS uses oklch color space for perceptual uniformity as defined in the design
- Basecoat component classes are used directly in templates — no additional component library needed


## Task Dependency Graph

```json
{
  "waves": [
    { "id": 0, "tasks": ["1.1"] },
    { "id": 1, "tasks": ["1.2", "1.3", "1.4"] },
    { "id": 2, "tasks": ["2.1", "2.2", "2.3", "2.4", "9.1", "9.3"] },
    { "id": 3, "tasks": ["3.1", "9.2"] },
    { "id": 4, "tasks": ["3.2", "3.3", "3.4", "4.3"] },
    { "id": 5, "tasks": ["4.1", "4.2", "5.1", "5.2", "5.3", "5.4", "5.5", "5.6"] },
    { "id": 6, "tasks": ["7.1", "7.2", "7.3", "7.5", "7.6", "7.7", "7.10"] },
    { "id": 7, "tasks": ["7.4", "7.8", "7.9", "8.1", "8.2", "8.3", "8.4"] },
    { "id": 8, "tasks": ["8.5", "8.6", "8.7", "11.1", "11.2"] },
    { "id": 9, "tasks": ["12.1", "13.1", "13.2", "13.3"] },
    { "id": 10, "tasks": ["11.3", "11.4", "12.2", "12.3", "12.4", "12.5", "12.6"] },
    { "id": 11, "tasks": ["14.1", "14.2", "14.3", "15.1", "15.2", "15.3"] },
    { "id": 12, "tasks": ["17.1", "17.2", "17.3", "17.4"] }
  ]
}
```
