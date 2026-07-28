/**
 * CodeTypewriter module — character-by-character code rendering with scroll trigger.
 *
 * Renders code lines one character at a time with configurable speed and
 * pauses between lines. Animation starts when the element scrolls into view
 * using IntersectionObserver.
 *
 * Requirements: 15.1, 15.2, 15.3
 */

/**
 * CodeTypewriter class.
 *
 * Sequentially renders code characters with configurable speed (Req 15.1),
 * pauses between lines (Req 15.3), and starts animation when element
 * scrolls into view via IntersectionObserver (Req 15.2).
 */
export class CodeTypewriter {
  /**
   * @param {HTMLElement} element - The DOM element to render code into.
   * @param {string[]} codeLines - Array of code lines to type out.
   * @param {Object} [options] - Configuration options.
   * @param {number} [options.speed=50] - Milliseconds between each character.
   * @param {number} [options.pauseBetweenLines=1000] - Milliseconds to pause between lines.
   */
  constructor(element, codeLines, options = {}) {
    this.element = element;
    this.codeLines = codeLines || [];
    this.speed = options.speed ?? 50;
    this.pauseBetweenLines = options.pauseBetweenLines ?? 1000;

    this._currentLineIndex = 0;
    this._currentCharIndex = 0;
    this._timerId = null;
    this._isRunning = false;
    this._isPaused = false;
    this._observer = null;
    this._started = false;

    // Set up IntersectionObserver to start animation on scroll into view
    this._setupScrollTrigger();
  }

  /**
   * Set up IntersectionObserver to trigger animation start when element
   * enters the viewport (Requirement 15.2).
   * @private
   */
  _setupScrollTrigger() {
    if (
      typeof window === 'undefined' ||
      !('IntersectionObserver' in window)
    ) {
      // No observer support — show content immediately
      return;
    }

    this._observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting && !this._started) {
            this._started = true;
            this.start();
            this._observer.unobserve(this.element);
          }
        }
      },
      { threshold: 0.1 }
    );

    this._observer.observe(this.element);
  }

  /**
   * Start the typewriter animation from the beginning.
   * Clears the element content and types characters one by one.
   */
  start() {
    if (this._isRunning) {
      return;
    }

    this._isRunning = true;
    this._isPaused = false;
    this._currentLineIndex = 0;
    this._currentCharIndex = 0;

    // Clear the element content before starting
    this.element.textContent = '';

    this._typeNextChar();
  }

  /**
   * Pause the typewriter animation. Can be resumed by calling start().
   */
  pause() {
    this._isPaused = true;
    this._isRunning = false;
    if (this._timerId !== null) {
      clearTimeout(this._timerId);
      this._timerId = null;
    }
  }

  /**
   * Reset the typewriter to its initial state.
   * Stops any running animation and clears the element content.
   */
  reset() {
    this._isRunning = false;
    this._isPaused = false;
    this._currentLineIndex = 0;
    this._currentCharIndex = 0;
    this._started = false;
    if (this._timerId !== null) {
      clearTimeout(this._timerId);
      this._timerId = null;
    }
    this.element.textContent = '';
  }

  /**
   * Disconnect the IntersectionObserver if active.
   */
  destroy() {
    this.pause();
    if (this._observer) {
      this._observer.disconnect();
      this._observer = null;
    }
  }

  /**
   * Type the next character, handling line endings and pauses.
   * @private
   */
  _typeNextChar() {
    if (!this._isRunning || this._isPaused) {
      return;
    }

    // Check if all lines have been typed
    if (this._currentLineIndex >= this.codeLines.length) {
      this._isRunning = false;
      return;
    }

    const currentLine = this.codeLines[this._currentLineIndex];

    // Check if we've finished the current line
    if (this._currentCharIndex >= currentLine.length) {
      // Move to next line
      this._currentLineIndex++;
      this._currentCharIndex = 0;

      // If there are more lines, add a newline and pause
      if (this._currentLineIndex < this.codeLines.length) {
        this.element.textContent += '\n';

        // Pause between lines (Requirement 15.3)
        this._timerId = setTimeout(() => {
          this._timerId = null;
          this._typeNextChar();
        }, this.pauseBetweenLines);
      } else {
        // All lines complete
        this._isRunning = false;
      }
      return;
    }

    // Type the next character
    this.element.textContent += currentLine[this._currentCharIndex];
    this._currentCharIndex++;

    // Schedule next character
    this._timerId = setTimeout(() => {
      this._timerId = null;
      this._typeNextChar();
    }, this.speed);
  }
}
