import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { CodeTypewriter } from '../../src/js/code-typewriter.js';

/**
 * Unit tests for CodeTypewriter module.
 * Tests character-by-character rendering, configurable speed,
 * pause between lines, scroll trigger, and control methods.
 */

function createElement() {
  const el = document.createElement('code');
  el.setAttribute('data-typewriter', '');
  document.body.appendChild(el);
  return el;
}

describe('CodeTypewriter', () => {
  let element;

  beforeEach(() => {
    vi.useFakeTimers();
    element = createElement();
  });

  afterEach(() => {
    vi.useRealTimers();
    document.body.innerHTML = '';
  });

  describe('character-by-character rendering (Req 15.1)', () => {
    it('should type a single line character by character', () => {
      const typewriter = new CodeTypewriter(element, ['hello'], { speed: 50, pauseBetweenLines: 1000 });

      // Disconnect observer to avoid scroll trigger — manual start
      typewriter.destroy();
      typewriter._started = true;
      typewriter._isRunning = false;
      typewriter.start();

      // After 0ms - first character typed immediately
      expect(element.textContent).toBe('h');

      // After 50ms - second character
      vi.advanceTimersByTime(50);
      expect(element.textContent).toBe('he');

      // After 100ms - third character
      vi.advanceTimersByTime(50);
      expect(element.textContent).toBe('hel');

      // After 150ms - fourth character
      vi.advanceTimersByTime(50);
      expect(element.textContent).toBe('hell');

      // After 200ms - fifth character
      vi.advanceTimersByTime(50);
      expect(element.textContent).toBe('hello');
    });

    it('should clear element content before starting animation', () => {
      element.textContent = 'pre-existing content';
      const typewriter = new CodeTypewriter(element, ['abc'], { speed: 50, pauseBetweenLines: 1000 });
      typewriter.destroy();
      typewriter._started = true;
      typewriter._isRunning = false;
      typewriter.start();

      expect(element.textContent).toBe('a');
    });

    it('should handle empty lines array gracefully', () => {
      const typewriter = new CodeTypewriter(element, [], { speed: 50, pauseBetweenLines: 1000 });
      typewriter.destroy();
      typewriter._started = true;
      typewriter._isRunning = false;
      typewriter.start();

      expect(element.textContent).toBe('');
    });

    it('should handle empty string lines', () => {
      const typewriter = new CodeTypewriter(element, ['', 'hi'], { speed: 50, pauseBetweenLines: 1000 });
      typewriter.destroy();
      typewriter._started = true;
      typewriter._isRunning = false;
      typewriter.start();

      // First line is empty, should add newline and pause
      // After pause, should start typing 'hi'
      vi.advanceTimersByTime(1000);
      expect(element.textContent).toBe('\nh');

      vi.advanceTimersByTime(50);
      expect(element.textContent).toBe('\nhi');
    });
  });

  describe('configurable speed', () => {
    it('should respect custom speed option', () => {
      const typewriter = new CodeTypewriter(element, ['abc'], { speed: 100, pauseBetweenLines: 1000 });
      typewriter.destroy();
      typewriter._started = true;
      typewriter._isRunning = false;
      typewriter.start();

      expect(element.textContent).toBe('a');

      // At 50ms, should still be 'a' (speed is 100ms)
      vi.advanceTimersByTime(50);
      expect(element.textContent).toBe('a');

      // At 100ms, should be 'ab'
      vi.advanceTimersByTime(50);
      expect(element.textContent).toBe('ab');
    });

    it('should use default speed of 50ms when not specified', () => {
      const typewriter = new CodeTypewriter(element, ['xy']);
      typewriter.destroy();
      typewriter._started = true;
      typewriter._isRunning = false;
      typewriter.start();

      expect(element.textContent).toBe('x');
      vi.advanceTimersByTime(50);
      expect(element.textContent).toBe('xy');
    });
  });

  describe('pause between lines (Req 15.3)', () => {
    it('should pause for default 1000ms between lines', () => {
      const typewriter = new CodeTypewriter(element, ['a', 'b'], { speed: 50, pauseBetweenLines: 1000 });
      typewriter.destroy();
      typewriter._started = true;
      typewriter._isRunning = false;
      typewriter.start();

      // Type 'a'
      expect(element.textContent).toBe('a');

      // Finish first line — newline added, pause begins
      vi.advanceTimersByTime(50);
      expect(element.textContent).toBe('a\n');

      // During pause (500ms into 1000ms pause), no new content
      vi.advanceTimersByTime(500);
      expect(element.textContent).toBe('a\n');

      // After full pause (remaining 500ms), next line starts
      vi.advanceTimersByTime(500);
      expect(element.textContent).toBe('a\nb');
    });

    it('should respect custom pauseBetweenLines option', () => {
      const typewriter = new CodeTypewriter(element, ['x', 'y'], { speed: 50, pauseBetweenLines: 2000 });
      typewriter.destroy();
      typewriter._started = true;
      typewriter._isRunning = false;
      typewriter.start();

      // Type 'x', then line ends
      expect(element.textContent).toBe('x');
      vi.advanceTimersByTime(50);
      expect(element.textContent).toBe('x\n');

      // After 1000ms — still pausing
      vi.advanceTimersByTime(1000);
      expect(element.textContent).toBe('x\n');

      // After full 2000ms pause, 'y' starts
      vi.advanceTimersByTime(1000);
      expect(element.textContent).toBe('x\ny');
    });

    it('should type multiple lines with pauses between each', () => {
      const typewriter = new CodeTypewriter(element, ['a', 'b', 'c'], { speed: 50, pauseBetweenLines: 100 });
      typewriter.destroy();
      typewriter._started = true;
      typewriter._isRunning = false;
      typewriter.start();

      // Line 1: 'a'
      expect(element.textContent).toBe('a');

      // End of line 1 + newline + pause + line 2
      vi.advanceTimersByTime(50); // end of 'a', newline written
      vi.advanceTimersByTime(100); // pause ends, 'b' typed
      expect(element.textContent).toBe('a\nb');

      // End of line 2 + newline + pause + line 3
      vi.advanceTimersByTime(50); // end of 'b', newline written
      vi.advanceTimersByTime(100); // pause ends, 'c' typed
      expect(element.textContent).toBe('a\nb\nc');
    });
  });

  describe('scroll into view trigger (Req 15.2)', () => {
    it('should set up IntersectionObserver on construction', () => {
      const observeSpy = vi.fn();
      const unobserveSpy = vi.fn();
      const disconnectSpy = vi.fn();

      global.IntersectionObserver = vi.fn((callback) => ({
        observe: observeSpy,
        unobserve: unobserveSpy,
        disconnect: disconnectSpy,
        _callback: callback
      }));

      const el = createElement();
      const tw = new CodeTypewriter(el, ['test'], { speed: 50 });

      expect(observeSpy).toHaveBeenCalledWith(el);

      // Clean up
      tw.destroy();
      delete global.IntersectionObserver;
    });

    it('should start animation when element enters viewport', () => {
      let observerCallback;
      const unobserveSpy = vi.fn();

      global.IntersectionObserver = vi.fn((callback) => {
        observerCallback = callback;
        return {
          observe: vi.fn(),
          unobserve: unobserveSpy,
          disconnect: vi.fn()
        };
      });

      const el = createElement();
      const tw = new CodeTypewriter(el, ['hi'], { speed: 50 });

      // Simulate element entering viewport
      observerCallback([{ isIntersecting: true }]);

      // Animation should have started
      expect(el.textContent).toBe('h');

      vi.advanceTimersByTime(50);
      expect(el.textContent).toBe('hi');

      // Observer should unobserve after trigger
      expect(unobserveSpy).toHaveBeenCalledWith(el);

      tw.destroy();
      delete global.IntersectionObserver;
    });

    it('should not start animation when element is not intersecting', () => {
      let observerCallback;

      global.IntersectionObserver = vi.fn((callback) => {
        observerCallback = callback;
        return {
          observe: vi.fn(),
          unobserve: vi.fn(),
          disconnect: vi.fn()
        };
      });

      const el = createElement();
      const tw = new CodeTypewriter(el, ['hi'], { speed: 50 });

      // Simulate element NOT entering viewport
      observerCallback([{ isIntersecting: false }]);

      // Animation should NOT have started
      expect(el.textContent).not.toBe('h');

      tw.destroy();
      delete global.IntersectionObserver;
    });

    it('should only trigger animation once even if observed multiple times', () => {
      let observerCallback;
      const unobserveSpy = vi.fn();

      global.IntersectionObserver = vi.fn((callback) => {
        observerCallback = callback;
        return {
          observe: vi.fn(),
          unobserve: unobserveSpy,
          disconnect: vi.fn()
        };
      });

      const el = createElement();
      const tw = new CodeTypewriter(el, ['a'], { speed: 50 });

      // First intersection
      observerCallback([{ isIntersecting: true }]);
      expect(el.textContent).toBe('a');

      // Second intersection should not restart
      vi.advanceTimersByTime(50);
      observerCallback([{ isIntersecting: true }]);

      // Content should still be just 'a', not restarted
      expect(el.textContent).toBe('a');

      tw.destroy();
      delete global.IntersectionObserver;
    });
  });

  describe('control methods', () => {
    it('pause() should stop the animation', () => {
      const typewriter = new CodeTypewriter(element, ['abcdef'], { speed: 50, pauseBetweenLines: 1000 });
      typewriter.destroy();
      typewriter._started = true;
      typewriter._isRunning = false;
      typewriter.start();

      // Type a few characters
      expect(element.textContent).toBe('a');
      vi.advanceTimersByTime(50);
      expect(element.textContent).toBe('ab');

      // Pause
      typewriter.pause();

      // Time passes but no more characters typed
      vi.advanceTimersByTime(200);
      expect(element.textContent).toBe('ab');
    });

    it('reset() should clear content and stop animation', () => {
      const typewriter = new CodeTypewriter(element, ['hello'], { speed: 50, pauseBetweenLines: 1000 });
      typewriter.destroy();
      typewriter._started = true;
      typewriter._isRunning = false;
      typewriter.start();

      vi.advanceTimersByTime(100); // Type 'hel'
      expect(element.textContent).toBe('hel');

      typewriter.reset();
      expect(element.textContent).toBe('');

      // No more typing after reset
      vi.advanceTimersByTime(200);
      expect(element.textContent).toBe('');
    });

    it('start() should not restart if already running', () => {
      const typewriter = new CodeTypewriter(element, ['ab'], { speed: 50, pauseBetweenLines: 1000 });
      typewriter.destroy();
      typewriter._started = true;
      typewriter._isRunning = false;
      typewriter.start();

      expect(element.textContent).toBe('a');

      // Calling start again while running should do nothing
      typewriter.start();
      expect(element.textContent).toBe('a');

      vi.advanceTimersByTime(50);
      expect(element.textContent).toBe('ab');
    });

    it('destroy() should disconnect observer and stop animation', () => {
      const disconnectSpy = vi.fn();

      global.IntersectionObserver = vi.fn((callback) => ({
        observe: vi.fn(),
        unobserve: vi.fn(),
        disconnect: disconnectSpy
      }));

      const el = createElement();
      const tw = new CodeTypewriter(el, ['test'], { speed: 50 });

      tw.destroy();
      expect(disconnectSpy).toHaveBeenCalled();

      delete global.IntersectionObserver;
    });
  });
});
