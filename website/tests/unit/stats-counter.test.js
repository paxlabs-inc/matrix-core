import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { StatsCounter } from '../../src/js/stats-counter.js';

/**
 * Unit tests for StatsCounter module.
 * Tests smooth numeric animation from 0 to target value using requestAnimationFrame.
 *
 * Validates: Requirements 15.4
 */

describe('StatsCounter', () => {
  let element;

  beforeEach(() => {
    element = document.createElement('span');
    element.setAttribute('data-counter', '400');
    element.setAttribute('data-suffix', 'ms');
    element.textContent = '0';
    document.body.appendChild(element);

    // Mock requestAnimationFrame to use manual stepping
    vi.useFakeTimers();
    let rafId = 0;
    const rafCallbacks = new Map();

    vi.stubGlobal('requestAnimationFrame', (cb) => {
      rafId += 1;
      rafCallbacks.set(rafId, cb);
      return rafId;
    });

    vi.stubGlobal('cancelAnimationFrame', (id) => {
      rafCallbacks.delete(id);
    });

    // Helper to flush all pending rAF callbacks at a given time
    element._rafCallbacks = rafCallbacks;
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
    document.body.innerHTML = '';
  });

  /**
   * Simulate animation frames up to a given elapsed time.
   */
  function runAnimationFrames(element, startTime, endTime, stepMs = 16) {
    const callbacks = element._rafCallbacks;
    let currentTime = startTime;
    while (currentTime <= endTime) {
      // Execute any pending callbacks at the current time
      const entries = [...callbacks.entries()];
      for (const [id, cb] of entries) {
        callbacks.delete(id);
        cb(currentTime);
      }
      currentTime += stepMs;
      if (callbacks.size === 0) break;
    }
  }

  describe('constructor', () => {
    it('should store element, target value, and default options', () => {
      const counter = new StatsCounter(element, 400);
      expect(counter.element).toBe(element);
      expect(counter.targetValue).toBe(400);
      expect(counter.duration).toBe(2000);
      expect(counter.suffix).toBe('');
    });

    it('should accept custom duration and suffix options', () => {
      const counter = new StatsCounter(element, 100, { duration: 3000, suffix: '%' });
      expect(counter.duration).toBe(3000);
      expect(counter.suffix).toBe('%');
    });
  });

  describe('animate()', () => {
    it('should set element text to target value with suffix when animation completes', () => {
      const counter = new StatsCounter(element, 400, { duration: 2000, suffix: 'ms' });
      const startTime = 1000;

      vi.spyOn(performance, 'now').mockReturnValue(startTime);
      counter.animate();

      // Run frames until past duration
      runAnimationFrames(element, startTime, startTime + 2100);

      expect(element.textContent).toBe('400ms');
    });

    it('should animate from 0 to target value', () => {
      const counter = new StatsCounter(element, 100, { duration: 1000, suffix: '' });
      const startTime = 0;

      vi.spyOn(performance, 'now').mockReturnValue(startTime);
      counter.animate();

      // At time 0, should start
      const callbacks = element._rafCallbacks;
      const [, firstCb] = [...callbacks.entries()][0];
      callbacks.clear();
      firstCb(startTime); // first frame at t=0 → progress=0 → value=0

      expect(element.textContent).toBe('0');
    });

    it('should produce intermediate values during animation', () => {
      const counter = new StatsCounter(element, 100, { duration: 1000, suffix: '' });
      const startTime = 0;

      vi.spyOn(performance, 'now').mockReturnValue(startTime);
      counter.animate();

      // At 50% through duration, value should be between 0 and 100 (with easing)
      const callbacks = element._rafCallbacks;
      const [id, cb] = [...callbacks.entries()][0];
      callbacks.delete(id);
      cb(500); // halfway

      const value = parseInt(element.textContent);
      expect(value).toBeGreaterThan(0);
      expect(value).toBeLessThan(100);
    });

    it('should apply ease-out effect (higher values earlier in animation)', () => {
      const counter = new StatsCounter(element, 1000, { duration: 1000, suffix: '' });
      const startTime = 0;

      vi.spyOn(performance, 'now').mockReturnValue(startTime);
      counter.animate();

      // Capture value at 50% progress
      const callbacks = element._rafCallbacks;
      let [id, cb] = [...callbacks.entries()][0];
      callbacks.delete(id);
      cb(500); // 50% of duration

      const valueAtHalf = parseInt(element.textContent);
      // With ease-out cubic, at t=0.5: 1 - (0.5)^3 = 0.875
      // So value should be around 875 (87.5% of 1000)
      expect(valueAtHalf).toBeGreaterThan(500); // Definitely past linear midpoint
    });

    it('should only animate once (subsequent calls are no-ops)', () => {
      const counter = new StatsCounter(element, 50, { duration: 1000, suffix: '' });
      const startTime = 0;

      vi.spyOn(performance, 'now').mockReturnValue(startTime);
      counter.animate();

      // Complete the animation
      runAnimationFrames(element, startTime, startTime + 1100);
      expect(element.textContent).toBe('50');

      // Calling animate again should not restart
      element.textContent = 'changed';
      counter.animate();
      expect(element.textContent).toBe('changed'); // Not reset to 0 or re-animated
    });

    it('should handle zero target value', () => {
      const counter = new StatsCounter(element, 0, { duration: 2000, suffix: 'ms' });
      counter.animate();

      expect(element.textContent).toBe('0ms');
    });

    it('should handle zero duration by immediately setting target', () => {
      const counter = new StatsCounter(element, 100, { duration: 0, suffix: '%' });
      counter.animate();

      expect(element.textContent).toBe('100%');
    });

    it('should handle negative duration by immediately setting target', () => {
      const counter = new StatsCounter(element, 42, { duration: -500, suffix: '' });
      counter.animate();

      expect(element.textContent).toBe('42');
    });

    it('should work without suffix (empty string)', () => {
      const counter = new StatsCounter(element, 10, { duration: 1000, suffix: '' });
      const startTime = 0;

      vi.spyOn(performance, 'now').mockReturnValue(startTime);
      counter.animate();

      runAnimationFrames(element, startTime, startTime + 1100);
      expect(element.textContent).toBe('10');
    });

    it('should append suffix to displayed value', () => {
      const counter = new StatsCounter(element, 99, { duration: 1000, suffix: '%' });
      const startTime = 0;

      vi.spyOn(performance, 'now').mockReturnValue(startTime);
      counter.animate();

      runAnimationFrames(element, startTime, startTime + 1100);
      expect(element.textContent).toBe('99%');
    });
  });

  describe('cancel()', () => {
    it('should cancel an in-progress animation', () => {
      const counter = new StatsCounter(element, 100, { duration: 2000, suffix: '' });
      const startTime = 0;

      vi.spyOn(performance, 'now').mockReturnValue(startTime);
      counter.animate();

      // First frame
      const callbacks = element._rafCallbacks;
      const [id, cb] = [...callbacks.entries()][0];
      callbacks.delete(id);
      cb(500); // 25% of duration

      const intermediateValue = element.textContent;

      counter.cancel();

      // Verify no more frames execute (callbacks should be cancelled)
      expect(counter._animationId).toBeNull();
    });

    it('should not throw when called with no active animation', () => {
      const counter = new StatsCounter(element, 100);
      expect(() => counter.cancel()).not.toThrow();
    });
  });

  describe('reset()', () => {
    it('should reset element text to 0 with suffix', () => {
      const counter = new StatsCounter(element, 100, { duration: 1000, suffix: 'ms' });
      const startTime = 0;

      vi.spyOn(performance, 'now').mockReturnValue(startTime);
      counter.animate();

      runAnimationFrames(element, startTime, startTime + 1100);
      expect(element.textContent).toBe('100ms');

      counter.reset();
      expect(element.textContent).toBe('0ms');
    });

    it('should allow animate() to be called again after reset', () => {
      const counter = new StatsCounter(element, 50, { duration: 1000, suffix: '' });
      const startTime = 0;

      vi.spyOn(performance, 'now').mockReturnValue(startTime);
      counter.animate();

      runAnimationFrames(element, startTime, startTime + 1100);
      expect(element.textContent).toBe('50');

      counter.reset();
      expect(element.textContent).toBe('0');

      // Should be able to animate again
      vi.spyOn(performance, 'now').mockReturnValue(2000);
      counter.animate();

      runAnimationFrames(element, 2000, 3100);
      expect(element.textContent).toBe('50');
    });

    it('should cancel any running animation', () => {
      const counter = new StatsCounter(element, 100, { duration: 2000, suffix: '' });
      const startTime = 0;

      vi.spyOn(performance, 'now').mockReturnValue(startTime);
      counter.animate();

      counter.reset();
      expect(counter._animationId).toBeNull();
      expect(counter._started).toBe(false);
    });
  });
});
