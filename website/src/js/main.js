/**
 * Main entry point — initializes all JavaScript modules on DOMContentLoaded.
 *
 * Each module initialization is wrapped in a try/catch block for graceful
 * degradation: if any module fails, the error is logged to the console and
 * the page continues to render without that specific interaction.
 *
 * Requirements: 25.1
 */

import { ScrollAnimator } from './animations.js';
import { CodeTypewriter } from './code-typewriter.js';
import { StatsCounter } from './stats-counter.js';
import { StickyNav } from './nav.js';

document.addEventListener('DOMContentLoaded', () => {
  // Graceful-degradation guarantee: every module below is initialized inside
  // its own try/catch. A failure in any one (ScrollAnimator, StatsCounter,
  // CodeTypewriter, StickyNav) is logged to the console and isolated — it can
  // never abort this handler or prevent the remaining modules from running.
  // The page content is rendered server-side by Eleventy, so it stays fully
  // visible and accessible even if JavaScript fails entirely. (Requirement 25.1)

  // Initialize scroll-reveal animations
  try {
    const animator = new ScrollAnimator();
    animator.observe('[data-animate="fade-up"]', 'animate-fade-up', { threshold: 0.1 });
    animator.observe('[data-animate="fade-in"]', 'animate-fade-in', { threshold: 0.2 });
    animator.observe('[data-animate="slide-right"]', 'animate-slide-right');
  } catch (error) {
    console.error('[Matrix] ScrollAnimator failed to initialize:', error);
  }

  // Initialize stats counters
  try {
    document.querySelectorAll('[data-counter]').forEach((el) => {
      const counter = new StatsCounter(el, parseInt(el.dataset.counter, 10), {
        duration: 2000,
        suffix: el.dataset.suffix || ''
      });
      // Trigger animation when element scrolls into view
      const observer = new IntersectionObserver(([entry]) => {
        if (entry.isIntersecting) {
          counter.animate();
          observer.disconnect();
        }
      });
      observer.observe(el);
    });
  } catch (error) {
    console.error('[Matrix] StatsCounter failed to initialize:', error);
  }

  // Initialize code typewriter on hero section
  try {
    const codeEl = document.querySelector('[data-typewriter]');
    if (codeEl) {
      const lines = codeEl.dataset.lines
        ? codeEl.dataset.lines.split('|')
        : [];
      const typewriter = new CodeTypewriter(codeEl, lines, {
        speed: 50,
        pauseBetweenLines: 1000
      });
      typewriter.start();
    }
  } catch (error) {
    console.error('[Matrix] CodeTypewriter failed to initialize:', error);
  }

  // Initialize sticky navigation
  try {
    const nav = document.querySelector('[data-sticky-nav]');
    if (nav) {
      new StickyNav(nav).init();
    }
  } catch (error) {
    console.error('[Matrix] StickyNav failed to initialize:', error);
  }
});
