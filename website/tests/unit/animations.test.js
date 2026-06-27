import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { ScrollAnimator } from '../../src/js/animations.js';

/**
 * Unit tests for ScrollAnimator module.
 * Tests IntersectionObserver-based scroll-triggered animation behavior.
 *
 * Validates: Requirements 14.1, 14.2, 14.3, 14.4
 */

describe('ScrollAnimator', () => {
  let observerCallback;
  let observeSpy;
  let unobserveSpy;
  let disconnectSpy;
  let mockObserverInstance;

  beforeEach(() => {
    observerCallback = null;
    observeSpy = vi.fn();
    unobserveSpy = vi.fn();
    disconnectSpy = vi.fn();

    mockObserverInstance = {
      observe: observeSpy,
      unobserve: unobserveSpy,
      disconnect: disconnectSpy,
    };

    global.IntersectionObserver = vi.fn((callback) => {
      observerCallback = callback;
      return mockObserverInstance;
    });

    // Mock IntersectionObserverEntry with prototype.intersectionRatio
    global.IntersectionObserverEntry = class {
      constructor() {}
    };
    global.IntersectionObserverEntry.prototype.intersectionRatio = 0;

    // Default: reduced motion not preferred
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      configurable: true,
      value: vi.fn((query) => ({
        matches: false,
        media: query,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
      })),
    });
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  function createElements(count, animateValue) {
    const elements = [];
    for (let i = 0; i < count; i++) {
      const el = document.createElement('div');
      el.setAttribute('data-animate', animateValue);
      document.body.appendChild(el);
      elements.push(el);
    }
    return elements;
  }

  describe('initial hidden state (Req 14.3)', () => {
    it('should apply opacity-0 and translate-y-4 to observed elements', () => {
      const elements = createElements(3, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up');

      elements.forEach((el) => {
        expect(el.classList.contains('opacity-0')).toBe(true);
        expect(el.classList.contains('translate-y-4')).toBe(true);
      });
    });

    it('should observe all matching elements with IntersectionObserver', () => {
      const elements = createElements(5, 'fade-in');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-in"]', 'animate-fade-in');

      expect(observeSpy).toHaveBeenCalledTimes(5);
      elements.forEach((el) => {
        expect(observeSpy).toHaveBeenCalledWith(el);
      });
    });
  });

  describe('animation class applied on intersection (Req 14.2)', () => {
    it('should apply animation class when element isIntersecting is true', () => {
      const elements = createElements(1, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up');

      // Simulate intersection
      observerCallback(
        [{ isIntersecting: true, target: elements[0] }],
        mockObserverInstance
      );

      expect(elements[0].classList.contains('animate-fade-up')).toBe(true);
    });

    it('should remove opacity-0 and translate-y-4 when animated', () => {
      const elements = createElements(1, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up');

      // Verify initial hidden state
      expect(elements[0].classList.contains('opacity-0')).toBe(true);
      expect(elements[0].classList.contains('translate-y-4')).toBe(true);

      // Simulate intersection
      observerCallback(
        [{ isIntersecting: true, target: elements[0] }],
        mockObserverInstance
      );

      expect(elements[0].classList.contains('opacity-0')).toBe(false);
      expect(elements[0].classList.contains('translate-y-4')).toBe(false);
    });

    it('should not apply animation class when element is not intersecting', () => {
      const elements = createElements(1, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up');

      // Simulate NOT intersecting
      observerCallback(
        [{ isIntersecting: false, target: elements[0] }],
        mockObserverInstance
      );

      expect(elements[0].classList.contains('animate-fade-up')).toBe(false);
      expect(elements[0].classList.contains('opacity-0')).toBe(true);
      expect(elements[0].classList.contains('translate-y-4')).toBe(true);
    });
  });

  describe('observer.unobserve called after animation (Req 14.2)', () => {
    it('should call unobserve on the target element after applying animation', () => {
      const elements = createElements(1, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up');

      // Simulate intersection
      observerCallback(
        [{ isIntersecting: true, target: elements[0] }],
        mockObserverInstance
      );

      expect(unobserveSpy).toHaveBeenCalledWith(elements[0]);
    });

    it('should not unobserve when element is not intersecting', () => {
      const elements = createElements(1, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up');

      observerCallback(
        [{ isIntersecting: false, target: elements[0] }],
        mockObserverInstance
      );

      expect(unobserveSpy).not.toHaveBeenCalled();
    });
  });

  describe('prefers-reduced-motion (Req 14.4)', () => {
    it('should skip animation setup and show elements in final state when reduced motion is preferred', () => {
      // Override matchMedia to return reduced motion preference
      Object.defineProperty(window, 'matchMedia', {
        writable: true,
        configurable: true,
        value: vi.fn((query) => ({
          matches: query === '(prefers-reduced-motion: reduce)',
          media: query,
          addEventListener: vi.fn(),
          removeEventListener: vi.fn(),
        })),
      });

      const elements = createElements(2, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up');

      // Elements should have animation class applied immediately
      elements.forEach((el) => {
        expect(el.classList.contains('animate-fade-up')).toBe(true);
        expect(el.classList.contains('opacity-0')).toBe(false);
        expect(el.classList.contains('translate-y-4')).toBe(false);
      });

      // IntersectionObserver.observe should NOT have been called
      expect(observeSpy).not.toHaveBeenCalled();
    });
  });

  describe('IntersectionObserver not supported (Req 14.5)', () => {
    it('should show all elements in final state when IntersectionObserver is unavailable', () => {
      // Remove IntersectionObserver and IntersectionObserverEntry
      const origIO = global.IntersectionObserver;
      const origIOE = global.IntersectionObserverEntry;
      delete global.IntersectionObserver;
      delete global.IntersectionObserverEntry;

      const elements = createElements(2, 'slide-right');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="slide-right"]', 'animate-slide-right');

      // Elements should be visible without animation
      elements.forEach((el) => {
        expect(el.classList.contains('animate-slide-right')).toBe(true);
        expect(el.classList.contains('opacity-0')).toBe(false);
        expect(el.classList.contains('translate-y-4')).toBe(false);
      });

      // Restore for other tests
      global.IntersectionObserver = origIO;
      global.IntersectionObserverEntry = origIOE;
    });
  });

  describe('observe() with no matching elements', () => {
    it('should not throw when selector matches no elements', () => {
      const animator = new ScrollAnimator();
      expect(() => {
        animator.observe('[data-animate="nonexistent"]', 'animate-fade-up');
      }).not.toThrow();
    });

    it('should not create an observer when no elements match', () => {
      const animator = new ScrollAnimator();
      animator.observe('[data-animate="nonexistent"]', 'animate-fade-up');

      expect(observeSpy).not.toHaveBeenCalled();
    });
  });

  describe('disconnect()', () => {
    it('should disconnect all active observers', () => {
      createElements(1, 'fade-up');
      createElements(1, 'fade-in');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up');
      animator.observe('[data-animate="fade-in"]', 'animate-fade-in');

      animator.disconnect();

      expect(disconnectSpy).toHaveBeenCalledTimes(2);
    });

    it('should clear the internal observers array', () => {
      createElements(1, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up');

      animator.disconnect();

      expect(animator._observers).toEqual([]);
    });
  });

  describe('custom IntersectionObserver options', () => {
    it('should pass custom threshold to IntersectionObserver', () => {
      createElements(1, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up', { threshold: 0.5 });

      expect(global.IntersectionObserver).toHaveBeenCalledWith(
        expect.any(Function),
        expect.objectContaining({ threshold: 0.5 })
      );
    });

    it('should pass custom rootMargin to IntersectionObserver', () => {
      createElements(1, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up', { rootMargin: '0px' });

      expect(global.IntersectionObserver).toHaveBeenCalledWith(
        expect.any(Function),
        expect.objectContaining({ rootMargin: '0px' })
      );
    });

    it('should use default threshold 0.1 when not specified', () => {
      createElements(1, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up');

      expect(global.IntersectionObserver).toHaveBeenCalledWith(
        expect.any(Function),
        expect.objectContaining({ threshold: 0.1 })
      );
    });

    it('should use default rootMargin when not specified', () => {
      createElements(1, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up');

      expect(global.IntersectionObserver).toHaveBeenCalledWith(
        expect.any(Function),
        expect.objectContaining({ rootMargin: '0px 0px -50px 0px' })
      );
    });
  });

  describe('multiple elements — independent animation', () => {
    it('should animate each element independently when they enter viewport', () => {
      const elements = createElements(2, 'fade-up');

      const animator = new ScrollAnimator();
      animator.observe('[data-animate="fade-up"]', 'animate-fade-up');

      // Only first element enters viewport
      observerCallback(
        [{ isIntersecting: true, target: elements[0] }],
        mockObserverInstance
      );

      expect(elements[0].classList.contains('animate-fade-up')).toBe(true);
      expect(elements[1].classList.contains('animate-fade-up')).toBe(false);
      expect(elements[1].classList.contains('opacity-0')).toBe(true);

      // Second element enters viewport
      observerCallback(
        [{ isIntersecting: true, target: elements[1] }],
        mockObserverInstance
      );

      expect(elements[1].classList.contains('animate-fade-up')).toBe(true);
      expect(elements[1].classList.contains('opacity-0')).toBe(false);
    });
  });
});
