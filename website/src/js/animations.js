/**
 * ScrollAnimator — IntersectionObserver-based scroll-triggered animation module.
 *
 * Provides scroll-driven reveal animations for marketing page elements.
 * Elements start hidden (opacity-0, translate-y-4) and animate into view
 * exactly once when they enter the viewport.
 *
 * Requirements: 14.1, 14.2, 14.3, 14.4, 14.5, 25.1
 */

/**
 * Checks whether the user prefers reduced motion via the
 * prefers-reduced-motion media query.
 * @returns {boolean} true if reduced motion is preferred
 */
function prefersReducedMotion() {
  return (
    typeof window !== 'undefined' &&
    window.matchMedia &&
    window.matchMedia('(prefers-reduced-motion: reduce)').matches
  );
}

/**
 * Checks whether IntersectionObserver API is available in the browser.
 * @returns {boolean} true if IntersectionObserver is supported
 */
function isIntersectionObserverSupported() {
  return (
    typeof window !== 'undefined' &&
    'IntersectionObserver' in window &&
    'IntersectionObserverEntry' in window &&
    'intersectionRatio' in IntersectionObserverEntry.prototype
  );
}

/**
 * ScrollAnimator class.
 *
 * Uses IntersectionObserver to detect when elements enter the viewport,
 * applying a specified animation class exactly once and then unobserving
 * the element. Respects prefers-reduced-motion and gracefully degrades
 * when IntersectionObserver is unsupported.
 */
export class ScrollAnimator {
  /**
   * @param {Object} [options] - Configuration options
   * @param {number} [options.threshold=0.1] - Default visibility threshold (0-1)
   * @param {string} [options.rootMargin='0px 0px -50px 0px'] - Default root margin
   */
  constructor(options = {}) {
    this._defaultThreshold = options.threshold ?? 0.1;
    this._defaultRootMargin = options.rootMargin ?? '0px 0px -50px 0px';
    this._observers = [];
    this._reducedMotion = prefersReducedMotion();
    this._supported = isIntersectionObserverSupported();
  }

  /**
   * Observe elements matching a selector and apply an animation class
   * when they enter the viewport.
   *
   * @param {string} selector - CSS selector for elements to observe
   * @param {string} animationClass - CSS class to add when element is visible
   * @param {Object} [options] - Per-call IntersectionObserver options
   * @param {number} [options.threshold] - Visibility threshold (0-1)
   * @param {string} [options.rootMargin] - Root margin for observer
   */
  observe(selector, animationClass, options = {}) {
    const elements = document.querySelectorAll(selector);

    if (!elements || elements.length === 0) {
      return;
    }

    // If reduced motion is preferred, show all elements in final state immediately
    if (this._reducedMotion) {
      elements.forEach((el) => {
        el.classList.remove('opacity-0', 'translate-y-4');
        el.classList.add(animationClass);
      });
      return;
    }

    // If IntersectionObserver is not supported, show all elements in final state
    if (!this._supported) {
      elements.forEach((el) => {
        el.classList.remove('opacity-0', 'translate-y-4');
        el.classList.add(animationClass);
      });
      return;
    }

    const threshold = options.threshold ?? this._defaultThreshold;
    const rootMargin = options.rootMargin ?? this._defaultRootMargin;

    const observer = new IntersectionObserver(
      (entries, obs) => {
        entries.forEach((entry) => {
          if (entry.isIntersecting) {
            entry.target.classList.remove('opacity-0', 'translate-y-4');
            entry.target.classList.add(animationClass);
            obs.unobserve(entry.target);
          }
        });
      },
      { threshold, rootMargin }
    );

    // Set initial hidden state and observe each element
    elements.forEach((el) => {
      el.classList.add('opacity-0', 'translate-y-4');
      observer.observe(el);
    });

    this._observers.push(observer);
  }

  /**
   * Disconnect all active observers and stop watching elements.
   */
  disconnect() {
    this._observers.forEach((observer) => {
      observer.disconnect();
    });
    this._observers = [];
  }
}
