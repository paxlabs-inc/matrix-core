/**
 * StatsCounter module — animates numbers from 0 to a target value.
 *
 * Provides smooth counting animation using requestAnimationFrame.
 * Intended to be triggered when the element scrolls into view
 * (IntersectionObserver handled externally, e.g. in main.js).
 *
 * Usage:
 *   <span data-counter="400" data-suffix="ms">0</span>
 *
 *   const counter = new StatsCounter(el, 400, { duration: 2000, suffix: 'ms' });
 *   counter.animate();
 *
 * Requirements: 15.4
 */

/**
 * Easing function — ease-out cubic for a decelerating animation feel.
 * @param {number} t - Progress value between 0 and 1
 * @returns {number} Eased value between 0 and 1
 */
function easeOutCubic(t) {
  return 1 - Math.pow(1 - t, 3);
}

/**
 * StatsCounter class.
 *
 * Animates a numeric value from 0 to a target using requestAnimationFrame,
 * applying an ease-out curve for a polished deceleration effect. Supports
 * configurable duration and an optional suffix appended to the displayed value.
 */
export class StatsCounter {
  /**
   * @param {HTMLElement} element - The DOM element to render the counter value into
   * @param {number} targetValue - The final numeric value to animate towards
   * @param {Object} [options] - Configuration options
   * @param {number} [options.duration=2000] - Animation duration in milliseconds
   * @param {string} [options.suffix=''] - Optional suffix appended to the displayed value (e.g. "ms", "%")
   */
  constructor(element, targetValue, options = {}) {
    this.element = element;
    this.targetValue = targetValue;
    this.duration = options.duration ?? 2000;
    this.suffix = options.suffix ?? '';
    this._animationId = null;
    this._started = false;
  }

  /**
   * Start the counting animation from 0 to targetValue.
   * Uses requestAnimationFrame for smooth 60fps rendering.
   * Animation runs only once — subsequent calls are no-ops.
   */
  animate() {
    // Prevent multiple animation runs
    if (this._started) {
      return;
    }
    this._started = true;

    const startTime = performance.now();
    const target = this.targetValue;
    const duration = this.duration;
    const element = this.element;
    const suffix = this.suffix;

    // Handle edge case: zero target or zero/negative duration
    if (target === 0 || duration <= 0) {
      element.textContent = target + suffix;
      return;
    }

    const step = (currentTime) => {
      const elapsed = currentTime - startTime;
      const progress = Math.min(elapsed / duration, 1);
      const easedProgress = easeOutCubic(progress);
      const currentValue = Math.round(easedProgress * target);

      element.textContent = currentValue + suffix;

      if (progress < 1) {
        this._animationId = requestAnimationFrame(step);
      } else {
        // Ensure we land exactly on the target value
        element.textContent = target + suffix;
        this._animationId = null;
      }
    };

    this._animationId = requestAnimationFrame(step);
  }

  /**
   * Cancel any in-progress animation.
   */
  cancel() {
    if (this._animationId !== null) {
      cancelAnimationFrame(this._animationId);
      this._animationId = null;
    }
  }

  /**
   * Reset the counter to its initial state (0 + suffix).
   * Cancels any running animation and allows animate() to be called again.
   */
  reset() {
    this.cancel();
    this._started = false;
    if (this.element) {
      this.element.textContent = '0' + this.suffix;
    }
  }
}
