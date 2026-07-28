import eleventyLucideicons from "@grimlink/eleventy-plugin-lucide-icons";
import pluginRss from "@11ty/eleventy-plugin-rss";
import Image from "@11ty/eleventy-img";
import {
  validatePageMetadata,
  validateNavItem,
  validateFeatureEntry,
  validatePricingPlans,
  validateBlogPost,
} from "./src/js/validation.js";

export default function (eleventyConfig) {
  // ─── Build-time error reporting (Requirements 25.3, 25.4) ────────────
  // 25.4 — Missing layout/macro references: Eleventy fails the build by
  //   DEFAULT with a descriptive error that names the offending template and
  //   the missing layout/macro (Nunjucks throws a "template not found" /
  //   "Unable to call ... not defined" error and Eleventy propagates it as a
  //   non-zero exit). We intentionally do NOT suppress or wrap these errors —
  //   a broken reference must stop the build so it can never ship. No extra
  //   code is required for this guarantee; this comment documents the reliance
  //   on that default behavior.
  // 25.3 — Missing image assets: the `image` async shortcode below logs a
  //   clear "⚠️  [image] Could not optimize ..." warning and degrades to a
  //   bare <img> carrying alt text, so the build still passes while surfacing
  //   the problem. The missing-data-file check (eleventy.after, near the end
  //   of this file) similarly warns loudly about absent referenced data files.

  // Register plugins
  eleventyConfig.addPlugin(eleventyLucideicons);
  eleventyConfig.addPlugin(pluginRss);

  // Custom filter: where - filters array by property value
  eleventyConfig.addFilter("where", (array, key, value) => {
    if (!Array.isArray(array)) return [];
    return array.filter((item) => item[key] === value);
  });

  // Custom filter: date - formats a date using simple tokens
  eleventyConfig.addFilter("date", (value, format) => {
    const d = new Date(value);
    if (isNaN(d.getTime())) return value;

    const months = ["January", "February", "March", "April", "May", "June",
      "July", "August", "September", "October", "November", "December"];
    const year = d.getFullYear();
    const month = d.getMonth();
    const day = d.getDate();

    if (format === "YYYY-MM-DD") {
      return `${year}-${String(month + 1).padStart(2, "0")}-${String(day).padStart(2, "0")}`;
    }
    if (format === "MMMM D, YYYY") {
      return `${months[month]} ${day}, ${year}`;
    }
    // Default: ISO string
    return d.toISOString();
  });

  // Passthrough copy for static assets
  eleventyConfig.addPassthroughCopy({ "docs/src/assets": "assets" });

  // Passthrough copy for client-side JavaScript (ES modules).
  // base.njk loads `/js/main.js` as a native ES module, which in turn imports
  // the sibling modules (animations.js, code-typewriter.js, stats-counter.js,
  // nav.js). Copy the whole src/js tree to _site/js so those imports resolve
  // at runtime. Served directly — no bundling step (Requirement 18.x).
  eleventyConfig.addPassthroughCopy({ "src/js": "js" });

  // Passthrough copy for security headers config (Requirements 22.1–22.6).
  // Copied verbatim to the site root so hosts (Netlify/Cloudflare) apply it.
  eleventyConfig.addPassthroughCopy({ "docs/src/_headers": "_headers" });

  // ─── Asset Optimization Pipeline (Requirements 19.1–19.5) ────────────
  // Responsive image optimization using @11ty/eleventy-img + sharp.
  //
  // The `image` async shortcode generates modern, content-hashed image
  // variants (AVIF + WebP) at responsive widths and emits accessible
  // <picture>/srcset markup with explicit width/height to prevent layout
  // shift (CLS). Output is written to _site/assets/img/ with hashed
  // filenames for cache-busting (Cache-Control: immutable on the CDN).
  //
  // Usage in templates (Nunjucks):
  //   {% image "./docs/src/assets/images/hero.png", "Matrix dashboard", "100vw" %}
  //   {% image "./docs/src/assets/images/hero.png", "Matrix dashboard", "100vw", "eager" %}
  //
  //   - Default behavior: loading="lazy" + decoding="async" for below-the-fold
  //     images (lazy-loaded only when scrolled near the viewport).
  //   - Pass "eager" (4th arg) for above-the-fold hero/LCP images: this emits
  //     loading="eager" + fetchpriority="high" so the browser prioritises the
  //     download and does NOT defer it. Use sparingly — only for the single
  //     most important image visible on initial paint.
  //
  // The shortcode is defined unconditionally; when the site has no images yet
  // (or a referenced source is missing) it degrades gracefully by logging a
  // build warning and emitting a plain <img> tag with alt text so the build
  // still passes (Requirement 25.3).

  const IMAGE_WIDTHS = [640, 1024, 1440, 1920];
  const IMAGE_FORMATS = ["avif", "webp"];

  // Maps original asset paths → generated content-hashed output files
  // (Requirement 19.4). Populated as the `image` shortcode runs and flushed
  // to _site/assets/img/manifest.json after the build completes.
  const imageManifest = {};

  async function imageShortcode(src, alt = "", sizes = "100vw", mode = "lazy") {
    if (typeof alt !== "string") {
      throw new Error(`image shortcode: missing \`alt\` text for ${src}`);
    }

    const isEager = mode === "eager" || mode === true || mode === "true";

    try {
      const metadata = await Image(src, {
        widths: IMAGE_WIDTHS,
        formats: IMAGE_FORMATS,
        // Content-hashed filenames for cache-busting.
        outputDir: "./_site/assets/img/",
        urlPath: "/assets/img/",
      });

      // Record manifest entries: original path → hashed output URLs.
      imageManifest[src] = Object.values(metadata)
        .flat()
        .map((entry) => entry.url);

      const imageAttributes = {
        alt,
        sizes,
        loading: isEager ? "eager" : "lazy",
        decoding: "async",
      };
      if (isEager) {
        imageAttributes.fetchpriority = "high";
      }

      // generateHTML emits <picture> with AVIF/WebP <source> srcset entries
      // plus a fallback <img> carrying explicit width & height attributes.
      return Image.generateHTML(metadata, imageAttributes);
    } catch (err) {
      // Graceful degradation: never break the build over a missing/invalid
      // image. Log a warning and fall back to a bare <img> with alt text.
      console.warn(
        `⚠️  [image] Could not optimize "${src}": ${err.message}. ` +
          `Rendering fallback <img> with alt text.`,
      );
      const loadingAttr = isEager ? "eager" : "lazy";
      const priorityAttr = isEager ? ' fetchpriority="high"' : "";
      return `<img src="${src}" alt="${alt}" loading="${loadingAttr}" decoding="async"${priorityAttr}>`;
    }
  }

  eleventyConfig.addAsyncShortcode("image", imageShortcode);

  // Write the image asset manifest after the build (Requirement 19.4).
  // Skipped silently when no images were processed.
  eleventyConfig.on("eleventy.after", async () => {
    const entries = Object.keys(imageManifest);
    if (entries.length === 0) return;
    try {
      const fs = await import("node:fs/promises");
      const dir = "./_site/assets/img";
      await fs.mkdir(dir, { recursive: true });
      await fs.writeFile(
        `${dir}/manifest.json`,
        JSON.stringify(imageManifest, null, 2),
      );
      console.log(
        `🖼️  Image manifest written: ${entries.length} source image(s).`,
      );
    } catch (e) {
      console.warn(`⚠️  Could not write image manifest: ${e.message}`);
    }
  });

  // Register blog posts collection - sorted by date descending
  eleventyConfig.addCollection("posts", (collectionApi) => {
    return collectionApi
      .getFilteredByGlob("docs/src/blog/*.md")
      .sort((a, b) => b.date - a.date);
  });

  // Register use cases collection
  eleventyConfig.addCollection("useCases", (collectionApi) => {
    return collectionApi.getFilteredByGlob("docs/src/use-cases/*.md");
  });

  // ─── Data Validation (Requirements 26.1–26.5, 16.6) ──────────────────
  // Validate content data during the build process.
  // Errors are logged as warnings to aid content authors without breaking the build.

  // Collect page data via a validating collection pass
  const pageDataForValidation = [];

  eleventyConfig.addCollection("__validation", (collectionApi) => {
    const allItems = collectionApi.getAll();
    for (const item of allItems) {
      pageDataForValidation.push({
        inputPath: item.inputPath,
        data: item.data,
      });
    }
    return []; // Return empty - this collection is just for validation side-effect
  });

  eleventyConfig.on("eleventy.after", async () => {
    const warnings = [];

    // Validate all collected page data (title, description, blog posts)
    for (const { inputPath, data } of pageDataForValidation) {
      // Validate page metadata (title, description)
      if (data.title || data.description) {
        const metaErrors = validatePageMetadata(data);
        for (const err of metaErrors) {
          warnings.push(`[${inputPath}] ${err}`);
        }
      }

      // Validate blog posts (date, tags, excerpt)
      if (data.tags || data.excerpt || (data.layout && String(data.layout).includes("blog"))) {
        const blogErrors = validateBlogPost(data);
        for (const err of blogErrors) {
          warnings.push(`[${inputPath}] ${err}`);
        }
      }
    }

    // Validate navigation items from site data
    try {
      const siteDataPath = new URL("./docs/src/_data/site.js", import.meta.url).pathname;
      const siteModule = await import(siteDataPath);
      const siteData = siteModule.default || siteModule;
      if (siteData.nav && Array.isArray(siteData.nav)) {
        for (const item of siteData.nav) {
          const navErrors = validateNavItem(item);
          for (const err of navErrors) {
            warnings.push(`[navigation] ${err}`);
          }
        }
      }
    } catch (e) {
      // Site data may not be accessible; skip navigation validation
    }

    // Validate features data
    try {
      const featuresDataPath = new URL("./docs/src/_data/features.js", import.meta.url).pathname;
      const featuresModule = await import(featuresDataPath);
      const features = featuresModule.default || featuresModule;
      if (Array.isArray(features)) {
        for (const feature of features) {
          const featureErrors = validateFeatureEntry(feature);
          for (const err of featureErrors) {
            warnings.push(`[features] ${err}`);
          }
        }
      }
    } catch (e) {
      // Features data may not be accessible; skip feature validation
    }

    // Validate pricing plans data
    try {
      const pricingDataPath = new URL("./docs/src/_data/pricing.js", import.meta.url).pathname;
      const pricingModule = await import(pricingDataPath);
      const pricing = pricingModule.default || pricingModule;
      if (Array.isArray(pricing)) {
        const pricingErrors = validatePricingPlans(pricing);
        for (const err of pricingErrors) {
          warnings.push(`[pricing] ${err}`);
        }
      }
    } catch (e) {
      // Pricing data may not be accessible; skip pricing validation
    }

    // Output all validation warnings
    if (warnings.length > 0) {
      console.warn("\n⚠️  Content Validation Warnings:");
      for (const w of warnings) {
        console.warn(`   • ${w}`);
      }
      console.warn(`\n   Total: ${warnings.length} validation issue(s) found.\n`);
    }
  });

  // ─── Build-time check: referenced data files present (Requirement 25.3) ──
  // Surface a clear warning if any expected global data file is missing, so
  // authors notice a broken/renamed data source during the build instead of
  // silently rendering pages with empty nav/features/pricing. Missing data
  // files only warn (the build still completes); missing *templates/macros*
  // still hard-fail via Eleventy's default behavior (Requirement 25.4).
  eleventyConfig.on("eleventy.after", async () => {
    const fs = await import("node:fs/promises");
    const expectedDataFiles = [
      "docs/src/_data/site.js",
      "docs/src/_data/features.js",
      "docs/src/_data/pricing.js",
      "docs/src/_data/navigation.js",
    ];
    const missing = [];
    for (const rel of expectedDataFiles) {
      try {
        await fs.access(new URL(`./${rel}`, import.meta.url).pathname);
      } catch {
        missing.push(rel);
      }
    }
    if (missing.length > 0) {
      console.warn("\n⚠️  Missing referenced data file(s):");
      for (const m of missing) {
        console.warn(`   • ${m}`);
      }
      console.warn(
        `   Pages depending on these may render with empty content.\n`,
      );
    }
  });

  return {
    dir: {
      input: "docs/src",
      output: "_site",
    },
    templateFormats: ["njk", "md", "html"],
    markdownTemplateEngine: "njk",
    htmlTemplateEngine: "njk",
  };
}
