import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { StickyNav } from '../../src/js/nav.js';

/**
 * Unit tests for StickyNav module.
 * Tests scroll-aware hide/show, backdrop blur, throttled handling, and mobile toggle.
 */

function createNavElement() {
  const nav = document.createElement('header');
  nav.setAttribute('data-sticky-nav', '');
  nav.classList.add('fixed', 'top-0', 'left-0', 'right-0', 'z-50', 'transition-all', 'duration-300');

  // Mobile toggle button
  const toggle = document.createElement('button');
  toggle.setAttribute('data-mobile-toggle', '');
  toggle.setAttribute('aria-expanded', 'false');
  toggle.setAttribute('aria-label', 'Toggle navigation menu');
  nav.appendChild(toggle);

  // Mobile menu
  const menu = document.createElement('div');
  menu.setAttribute('data-mobile-menu', '');
  menu.setAttribute('aria-hidden', 'true');
  nav.appendChild(menu);

  document.body.appendChild(nav);
  return nav;
}

function simulateScroll(y) {
  Object.defineProperty(window, 'scrollY', { value: y, writable: true, configurable: true });
  window.dispatchEvent(new Event('scroll'));
}

describe('StickyNav', () => {
  let navElement;
  let stickyNav;

  beforeEach(() => {
    vi.useFakeTimers();
    Object.defineProperty(window, 'scrollY', { value: 0, writable: true, configurable: true });
    navElement = createNavElement();
    stickyNav = new StickyNav(navElement);
    stickyNav.init();
  });

  afterEach(() => {
    vi.useRealTimers();
    document.body.innerHTML = '';
  });

  describe('scroll-aware hide/show', () => {
    it('should hide nav when scrolling down past 100px threshold', () => {
      // Scroll down past threshold
      simulateScroll(150);
      vi.advanceTimersByTime(20);

      expect(navElement.style.transform).toBe('translateY(-100%)');
    });

    it('should not hide nav when scrolling down below 100px threshold', () => {
      simulateScroll(50);
      vi.advanceTimersByTime(20);

      expect(navElement.style.transform).not.toBe('translateY(-100%)');
    });

    it('should show nav when scrolling up', () => {
      // Scroll down to hide
      simulateScroll(200);
      vi.advanceTimersByTime(20);

      expect(navElement.style.transform).toBe('translateY(-100%)');

      // Scroll up to reveal
      simulateScroll(100);
      vi.advanceTimersByTime(20);

      expect(navElement.style.transform).toBe('translateY(0)');
    });

    it('should show nav when scrolled back to top', () => {
      // Scroll down
      simulateScroll(200);
      vi.advanceTimersByTime(20);

      // Scroll back to top
      simulateScroll(0);
      vi.advanceTimersByTime(20);

      expect(navElement.style.transform).toBe('translateY(0)');
    });
  });

  describe('backdrop blur and border', () => {
    it('should apply backdrop blur and border when scrolled past 0', () => {
      simulateScroll(10);
      vi.advanceTimersByTime(20);

      expect(navElement.classList.contains('backdrop-blur-lg')).toBe(true);
      expect(navElement.classList.contains('border-b')).toBe(true);
      expect(navElement.classList.contains('border-border')).toBe(true);
    });

    it('should remove backdrop blur and border when at top (scrollY = 0)', () => {
      // Scroll down first to add blur
      simulateScroll(10);
      vi.advanceTimersByTime(20);

      // Scroll back to top
      simulateScroll(0);
      vi.advanceTimersByTime(20);

      expect(navElement.classList.contains('backdrop-blur-lg')).toBe(false);
      expect(navElement.classList.contains('border-b')).toBe(false);
      expect(navElement.classList.contains('border-border')).toBe(false);
    });

    it('should not have blur/border initially at scroll position 0', () => {
      expect(navElement.classList.contains('backdrop-blur-lg')).toBe(false);
      expect(navElement.classList.contains('border-b')).toBe(false);
    });
  });

  describe('mobile menu toggle', () => {
    it('should open mobile menu on toggle click (aria-hidden true → false)', () => {
      const toggle = navElement.querySelector('[data-mobile-toggle]');
      const menu = navElement.querySelector('[data-mobile-menu]');

      toggle.click();

      expect(menu.getAttribute('aria-hidden')).toBe('false');
      expect(toggle.getAttribute('aria-expanded')).toBe('true');
    });

    it('should close mobile menu on second toggle click', () => {
      const toggle = navElement.querySelector('[data-mobile-toggle]');
      const menu = navElement.querySelector('[data-mobile-menu]');

      // Open
      toggle.click();
      expect(menu.getAttribute('aria-hidden')).toBe('false');

      // Close
      toggle.click();
      expect(menu.getAttribute('aria-hidden')).toBe('true');
      expect(toggle.getAttribute('aria-expanded')).toBe('false');
    });

    it('should not throw if toggle or menu elements are missing', () => {
      document.body.innerHTML = '';
      const emptyNav = document.createElement('header');
      emptyNav.setAttribute('data-sticky-nav', '');
      document.body.appendChild(emptyNav);

      const nav = new StickyNav(emptyNav);
      expect(() => nav.init()).not.toThrow();
    });

    it('should close the open mobile menu when Escape is pressed', () => {
      const toggle = navElement.querySelector('[data-mobile-toggle]');
      const menu = navElement.querySelector('[data-mobile-menu]');

      // Open the menu
      toggle.click();
      expect(menu.getAttribute('aria-hidden')).toBe('false');

      // Press Escape
      document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));

      expect(menu.getAttribute('aria-hidden')).toBe('true');
      expect(toggle.getAttribute('aria-expanded')).toBe('false');
    });

    it('should return focus to the toggle button after closing via Escape', () => {
      const toggle = navElement.querySelector('[data-mobile-toggle]');
      const focusSpy = vi.spyOn(toggle, 'focus');

      // Open then close via Escape
      toggle.click();
      document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));

      expect(focusSpy).toHaveBeenCalled();
    });

    it('should ignore Escape when the mobile menu is already closed', () => {
      const toggle = navElement.querySelector('[data-mobile-toggle]');
      const menu = navElement.querySelector('[data-mobile-menu]');
      const focusSpy = vi.spyOn(toggle, 'focus');

      // Menu starts closed; Escape should be a no-op
      document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));

      expect(menu.getAttribute('aria-hidden')).toBe('true');
      expect(focusSpy).not.toHaveBeenCalled();
    });
  });

  describe('throttle behavior', () => {
    it('should not process every scroll event immediately (throttled at 16ms)', () => {
      // Rapid scroll events
      simulateScroll(50);
      simulateScroll(110);
      simulateScroll(200);

      // Before timer advances, the throttle should batch
      // After first call fires, only one more fires after delay
      vi.advanceTimersByTime(16);

      // The nav should reflect the latest scroll position after throttle fires
      expect(navElement.style.transform).toBe('translateY(-100%)');
    });
  });
});
