/**
 * StickyNav module — scroll-aware hide/show navigation behavior.
 *
 * Implements:
 * - Hide on scroll down past 100px, show on scroll up (Requirements 13.2, 13.3)
 * - Backdrop blur and border when page is scrolled (Requirement 13.4)
 * - Throttled scroll handler at 16ms / 60fps with passive listener (Requirement 13.6)
 * - Mobile menu toggle with ARIA attribute updates (Requirement 13.5)
 * - Sticky positioning at top of viewport (Requirement 13.1)
 * - Keyboard accessibility: close mobile menu on Escape and return focus to
 *   the toggle button (Requirements 20.2, 20.4)
 */

/**
 * Throttle a function to fire at most once every `limit` milliseconds.
 * @param {Function} fn - The function to throttle.
 * @param {number} limit - Minimum milliseconds between calls.
 * @returns {Function}
 */
function throttle(fn, limit) {
  let lastCall = 0;
  let scheduled = null;

  return function throttled(...args) {
    const now = Date.now();
    const remaining = limit - (now - lastCall);

    if (remaining <= 0) {
      if (scheduled !== null) {
        clearTimeout(scheduled);
        scheduled = null;
      }
      lastCall = now;
      fn.apply(this, args);
    } else if (scheduled === null) {
      scheduled = setTimeout(() => {
        lastCall = Date.now();
        scheduled = null;
        fn.apply(this, args);
      }, remaining);
    }
  };
}

export class StickyNav {
  /**
   * @param {HTMLElement} navElement - The nav element with [data-sticky-nav].
   */
  constructor(navElement) {
    this.nav = navElement;
    this.lastScrollY = 0;
    this.isHidden = false;
    this.scrollThreshold = 100;
    this.throttleInterval = 16; // ~60fps
  }

  /**
   * Initialize scroll handling and mobile menu toggle.
   */
  init() {
    this._bindScroll();
    this._bindMobileToggle();
  }

  /**
   * Attach the throttled scroll listener (passive for performance).
   * @private
   */
  _bindScroll() {
    const handler = throttle(() => this._handleScroll(), this.throttleInterval);
    window.addEventListener('scroll', handler, { passive: true });
  }

  /**
   * Handle scroll events: apply blur/border when scrolled, hide/show on direction.
   * @private
   */
  _handleScroll() {
    const currentScrollY = window.scrollY;

    // Apply backdrop blur and border when page is scrolled past 0
    if (currentScrollY > 0) {
      this.nav.classList.add('backdrop-blur-lg', 'border-b', 'border-border');
    } else {
      this.nav.classList.remove('backdrop-blur-lg', 'border-b', 'border-border');
    }

    // Hide on scroll down past threshold, show on scroll up
    if (currentScrollY > this.lastScrollY && currentScrollY > this.scrollThreshold) {
      if (!this.isHidden) {
        this.nav.style.transform = 'translateY(-100%)';
        this.isHidden = true;
      }
    } else {
      if (this.isHidden) {
        this.nav.style.transform = 'translateY(0)';
        this.isHidden = false;
      }
    }

    this.lastScrollY = currentScrollY;
  }

  /**
   * Bind mobile menu toggle button to show/hide mobile menu with ARIA updates.
   * Also supports closing the menu via the Escape key, returning focus to the
   * toggle button for keyboard accessibility.
   * @private
   */
  _bindMobileToggle() {
    const toggle = this.nav.querySelector('[data-mobile-toggle]');
    const menu = this.nav.querySelector('[data-mobile-menu]');

    if (!toggle || !menu) {
      return;
    }

    this._toggle = toggle;
    this._menu = menu;

    toggle.addEventListener('click', () => {
      const isCurrentlyHidden = menu.getAttribute('aria-hidden') === 'true';

      if (isCurrentlyHidden) {
        this._openMobileMenu();
      } else {
        this._closeMobileMenu();
      }
    });

    // Close the menu on Escape and return focus to the toggle button.
    document.addEventListener('keydown', (event) => {
      if (event.key === 'Escape' && menu.getAttribute('aria-hidden') === 'false') {
        this._closeMobileMenu();
        toggle.focus();
      }
    });
  }

  /**
   * Open the mobile menu and update ARIA state.
   * @private
   */
  _openMobileMenu() {
    if (!this._menu || !this._toggle) {
      return;
    }
    this._menu.setAttribute('aria-hidden', 'false');
    this._toggle.setAttribute('aria-expanded', 'true');
    this._menu.style.display = '';
  }

  /**
   * Close the mobile menu and update ARIA state.
   * @private
   */
  _closeMobileMenu() {
    if (!this._menu || !this._toggle) {
      return;
    }
    this._menu.setAttribute('aria-hidden', 'true');
    this._toggle.setAttribute('aria-expanded', 'false');
  }
}
