import '@testing-library/jest-dom'

// Happy DOM exposes <dialog> but does not implement its imperative API.
// Astryx Dialog intentionally uses the native methods, so mirror the browser
// state transitions closely enough for component tests to exercise the real
// open/close path.
const dialogPrototype = Object.getPrototypeOf(document.createElement('dialog')) as {
  show?: (this: HTMLDialogElement) => void
  showModal?: (this: HTMLDialogElement) => void
  close?: (this: HTMLDialogElement, returnValue?: string) => void
}

if (typeof dialogPrototype.showModal !== 'function') {
  dialogPrototype.showModal = function showModal() {
    this.open = true
    this.setAttribute('open', '')
  }
}

if (typeof dialogPrototype.show !== 'function') {
  dialogPrototype.show = function show() {
    this.open = true
    this.setAttribute('open', '')
  }
}

if (typeof dialogPrototype.close !== 'function') {
  dialogPrototype.close = function close(returnValue = '') {
    this.returnValue = returnValue
    this.open = false
    this.removeAttribute('open')
    this.dispatchEvent(new Event('close'))
  }
}
