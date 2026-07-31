/* ============================================================
   Matrix Cookie Consent — drop-in, framework-agnostic
   Writes/reads the `mx_consent` cookie (see Cookie Policy).
   ------------------------------------------------------------
   USAGE
     <script src="cookie-consent.js" defer></script>

   GATE NON-ESSENTIAL SCRIPTS (don't run until consent):
     <script type="text/plain" data-consent="analytics"
             src="https://example-analytics/snippet.js"></script>
   Inline is also supported:
     <script type="text/plain" data-consent="analytics">
       // analytics init code here
     </script>

   PUBLIC API (window.MatrixConsent):
     .get()             -> { necessary, functional, analytics, ts, v } | null
     .has()             -> boolean (has the user made a choice?)
     .allowed(cat)      -> boolean for 'functional' | 'analytics'
     .openPreferences() -> reopen the settings dialog
     .onChange(fn)      -> subscribe; fn receives the consent object
     .reset()           -> clear consent and show the banner again

   Add a "Cookie settings" trigger anywhere:
     <a href="#" data-cookie-settings>Cookie settings</a>
   ============================================================ */
;(function () {
  'use strict'

  var COOKIE = 'mx_consent'
  var VERSION = 1 // bump to re-prompt all users after policy changes
  var MAX_AGE = 60 * 60 * 24 * 365 // 12 months, matches Cookie Policy
  var CATEGORIES = ['functional', 'analytics'] // 'necessary' is always true

  // ---- cookie helpers --------------------------------------------------
  function writeCookie(value) {
    var secure = location.protocol === 'https:' ? '; Secure' : ''
    document.cookie =
      COOKIE +
      '=' +
      encodeURIComponent(value) +
      '; Max-Age=' +
      MAX_AGE +
      '; Path=/; SameSite=Lax' +
      secure
  }
  function readCookie() {
    var m = document.cookie.match(new RegExp('(?:^|; )' + COOKIE + '=([^;]*)'))
    if (!m) return null
    try {
      return JSON.parse(decodeURIComponent(m[1]))
    } catch (e) {
      return null
    }
  }
  function clearCookie() {
    document.cookie = COOKIE + '=; Max-Age=0; Path=/; SameSite=Lax'
  }

  // Respect Global Privacy Control / Do Not Track as an opt-out default.
  function privacySignal() {
    try {
      return (
        navigator.globalPrivacyControl === true ||
        navigator.doNotTrack === '1' ||
        window.doNotTrack === '1'
      )
    } catch (e) {
      return false
    }
  }

  var listeners = []
  function notify(consent) {
    listeners.forEach(function (fn) {
      try {
        fn(consent)
      } catch (e) {}
    })
  }

  // ---- activate gated scripts -----------------------------------------
  function activateScripts(consent) {
    var nodes = document.querySelectorAll('script[type="text/plain"][data-consent]')
    nodes.forEach(function (node) {
      var cat = node.getAttribute('data-consent')
      if (!consent[cat]) return
      if (node.dataset.activated) return
      var s = document.createElement('script')
      Array.prototype.forEach.call(node.attributes, function (a) {
        if (a.name === 'type' || a.name === 'data-consent') return
        s.setAttribute(a.name, a.value)
      })
      if (node.src) {
        s.src = node.src
      } else {
        s.textContent = node.textContent
      }
      node.dataset.activated = '1'
      node.parentNode.insertBefore(s, node.nextSibling)
    })
  }

  // ---- persist + apply -------------------------------------------------
  function save(choice) {
    var consent = {
      v: VERSION,
      necessary: true,
      functional: !!choice.functional,
      analytics: !!choice.analytics,
      ts: Date.now(),
    }
    writeCookie(JSON.stringify(consent))
    activateScripts(consent)
    notify(consent)
    return consent
  }

  // ---- UI --------------------------------------------------------------
  var STYLE =
    '\
  .mxc-root{position:fixed;inset:0;z-index:2147483000;pointer-events:none;font-family:"Spline Sans",-apple-system,BlinkMacSystemFont,sans-serif}\
  .mxc-root *{box-sizing:border-box}\
  .mxc-scrim{position:absolute;inset:0;background:rgba(15,17,28,.42);opacity:0;transition:opacity .25s;pointer-events:none}\
  .mxc-root.mxc-modal .mxc-scrim{opacity:1;pointer-events:auto}\
  .mxc-panel{position:absolute;left:50%;bottom:24px;transform:translateX(-50%) translateY(12px);width:min(560px,calc(100% - 32px));\
    background:#fff;border:1px solid #e7e8ee;border-radius:16px;box-shadow:0 12px 48px rgba(20,23,40,.18);\
    padding:22px 24px;pointer-events:auto;opacity:0;transition:opacity .25s,transform .25s}\
  .mxc-root.mxc-show .mxc-panel{opacity:1;transform:translateX(-50%) translateY(0)}\
  .mxc-root.mxc-modal .mxc-panel{top:50%;bottom:auto;transform:translate(-50%,-46%);max-height:90vh;overflow:auto}\
  .mxc-root.mxc-modal.mxc-show .mxc-panel{transform:translate(-50%,-50%)}\
  .mxc-eyebrow{display:inline-flex;align-items:center;gap:7px;font-size:11px;letter-spacing:.1em;text-transform:uppercase;\
    color:#1c2e85;font-weight:600;background:#eef1fb;padding:5px 10px;border-radius:999px;margin-bottom:12px}\
  .mxc-eyebrow i{width:6px;height:6px;border-radius:50%;background:#2841B8;display:inline-block}\
  .mxc-title{font-family:"Fraunces",Georgia,serif;font-weight:600;font-size:1.28rem;margin:0 0 6px;color:#15171f;letter-spacing:-.01em}\
  .mxc-body{font-size:14px;line-height:1.6;color:#4a4f5e;margin:0 0 16px}\
  .mxc-body a{color:#1c2e85}\
  .mxc-opts{margin:0 0 16px;border-top:1px solid #eef0f5}\
  .mxc-opt{display:flex;align-items:flex-start;gap:12px;padding:13px 2px;border-bottom:1px solid #eef0f5}\
  .mxc-opt h4{margin:0 0 2px;font-size:14px;font-weight:600;color:#15171f}\
  .mxc-opt p{margin:0;font-size:12.5px;line-height:1.5;color:#888da0}\
  .mxc-sw{position:relative;flex:none;width:42px;height:24px;margin-top:2px}\
  .mxc-sw input{opacity:0;width:0;height:0;position:absolute}\
  .mxc-track{position:absolute;inset:0;background:#d6d9e6;border-radius:999px;transition:background .18s;cursor:pointer}\
  .mxc-track:before{content:"";position:absolute;height:18px;width:18px;left:3px;top:3px;background:#fff;border-radius:50%;transition:transform .18s;box-shadow:0 1px 3px rgba(0,0,0,.2)}\
  .mxc-sw input:checked + .mxc-track{background:#2841B8}\
  .mxc-sw input:checked + .mxc-track:before{transform:translateX(18px)}\
  .mxc-sw input:disabled + .mxc-track{background:#2841B8;opacity:.5;cursor:not-allowed}\
  .mxc-actions{display:flex;flex-wrap:wrap;gap:10px}\
  .mxc-btn{flex:1 1 auto;min-width:120px;font-family:inherit;font-size:14px;font-weight:600;border-radius:10px;padding:11px 16px;cursor:pointer;border:1px solid transparent;transition:filter .15s,background .15s,border-color .15s}\
  .mxc-btn-primary{background:#2841B8;color:#fff}\
  .mxc-btn-primary:hover{filter:brightness(1.08)}\
  .mxc-btn-ghost{background:#fff;color:#1c2e85;border-color:#dde2f5}\
  .mxc-btn-ghost:hover{background:#eef1fb}\
  .mxc-link{background:none;border:none;color:#888da0;font-size:13px;cursor:pointer;text-decoration:underline;padding:6px;font-family:inherit}\
  .mxc-signal{font-size:12px;color:#7a4a12;background:#fff7ed;border:1px solid #f0c894;border-radius:8px;padding:8px 11px;margin:0 0 14px}\
  @media (prefers-color-scheme:dark){\
    .mxc-panel{background:#16181f;border-color:#2a2d3a}\
    .mxc-title{color:#f3f4f8}.mxc-body{color:#b7bbc9}.mxc-opt h4{color:#f3f4f8}\
    .mxc-opt,.mxc-opts{border-color:#262936}\
    .mxc-btn-ghost{background:#16181f;color:#aeb8ee;border-color:#343849}.mxc-btn-ghost:hover{background:#1d2030}}\
  @media (max-width:480px){.mxc-btn{flex:1 1 100%}}'

  function el(html) {
    var d = document.createElement('div')
    d.innerHTML = html.trim()
    return d.firstChild
  }

  var root,
    showOptions = false

  function render() {
    var existing = readCookie()
    var fnOn = existing ? existing.functional : true // default functional on
    var anOn = existing ? existing.analytics : !privacySignal() // default off if GPC/DNT

    root = el(
      '<div class="mxc-root" role="dialog" aria-modal="false" aria-label="Cookie consent"></div>',
    )
    var style = document.createElement('style')
    style.textContent = STYLE
    root.appendChild(style)
    root.appendChild(el('<div class="mxc-scrim" data-act="dismiss"></div>'))

    var signal = privacySignal()
      ? '<p class="mxc-signal">We detected a Global Privacy Control / Do&nbsp;Not&nbsp;Track signal, so analytics is switched off by default. You can change this below.</p>'
      : ''

    var panel = el(
      '\
      <div class="mxc-panel">\
        <span class="mxc-eyebrow"><i></i> Your privacy</span>\
        <h3 class="mxc-title">We use cookies</h3>\
        <p class="mxc-body">Matrix uses strictly necessary cookies to run, and — with your consent — functional and analytics cookies to improve the experience. See our <a href="cookie-policy.html">Cookie Policy</a> and <a href="privacy-policy.html">Privacy Policy</a>.</p>\
        ' +
        signal +
        '\
        <div class="mxc-opts" data-opts hidden>\
          <div class="mxc-opt">\
            <label class="mxc-sw"><input type="checkbox" checked disabled><span class="mxc-track"></span></label>\
            <div><h4>Strictly necessary</h4><p>Required for security, sessions, and connecting your wallet. Always on.</p></div>\
          </div>\
          <div class="mxc-opt">\
            <label class="mxc-sw"><input type="checkbox" data-cat="functional" ' +
        (fnOn ? 'checked' : '') +
        '><span class="mxc-track"></span></label>\
            <div><h4>Functional</h4><p>Remembers preferences such as language and interface settings.</p></div>\
          </div>\
          <div class="mxc-opt">\
            <label class="mxc-sw"><input type="checkbox" data-cat="analytics" ' +
        (anOn ? 'checked' : '') +
        '><span class="mxc-track"></span></label>\
            <div><h4>Analytics</h4><p>Aggregated, privacy-respecting usage measurement to help us improve.</p></div>\
          </div>\
        </div>\
        <div class="mxc-actions">\
          <button class="mxc-btn mxc-btn-ghost" data-act="reject">Reject non-essential</button>\
          <button class="mxc-btn mxc-btn-ghost" data-act="toggle">Customize</button>\
          <button class="mxc-btn mxc-btn-primary" data-act="accept">Accept all</button>\
        </div>\
        <div style="text-align:center;margin-top:6px"><button class="mxc-link" data-act="save" hidden>Save my choices</button></div>\
      </div>',
    )
    root.appendChild(panel)
    document.body.appendChild(root)
    requestAnimationFrame(function () {
      root.classList.add('mxc-show')
    })

    root.addEventListener('click', onClick)
  }

  function setOptionsView(on) {
    showOptions = on
    var opts = root.querySelector('[data-opts]')
    var saveBtn = root.querySelector('[data-act="save"]')
    var toggleBtn = root.querySelector('[data-act="toggle"]')
    opts.hidden = !on
    saveBtn.hidden = !on
    toggleBtn.textContent = on ? 'Hide options' : 'Customize'
    root.classList.toggle('mxc-modal', on)
  }

  function readToggles() {
    var out = { functional: false, analytics: false }
    CATEGORIES.forEach(function (c) {
      var box = root.querySelector('[data-cat="' + c + '"]')
      out[c] = box ? box.checked : false
    })
    return out
  }

  function close() {
    if (!root) return
    root.classList.remove('mxc-show')
    setTimeout(function () {
      if (root) {
        root.remove()
        root = null
      }
    }, 260)
  }

  function onClick(e) {
    var t = e.target.closest('[data-act]')
    if (!t) return
    var act = t.getAttribute('data-act')
    if (act === 'accept') {
      save({ functional: true, analytics: true })
      close()
    } else if (act === 'reject') {
      save({ functional: false, analytics: false })
      close()
    } else if (act === 'toggle') {
      setOptionsView(!showOptions)
    } else if (act === 'save') {
      save(readToggles())
      close()
    } else if (act === 'dismiss') {
      // dismissing the scrim in customize view = treat as no non-essential consent
      if (!readCookie()) {
        save({ functional: false, analytics: false })
      }
      close()
    }
  }

  // ---- boot ------------------------------------------------------------
  function boot() {
    var existing = readCookie()
    if (existing && existing.v === VERSION) {
      activateScripts(existing) // honor stored choice on every load
      notify(existing)
    } else {
      if (existing) clearCookie() // stale version -> re-prompt
      render()
    }
    // global "Cookie settings" triggers
    document.addEventListener('click', function (e) {
      var trig = e.target.closest('[data-cookie-settings]')
      if (trig) {
        e.preventDefault()
        api.openPreferences()
      }
    })
  }

  // ---- public API ------------------------------------------------------
  var api = {
    get: function () {
      return readCookie()
    },
    has: function () {
      var c = readCookie()
      return !!(c && c.v === VERSION)
    },
    allowed: function (cat) {
      var c = readCookie()
      return !!(c && c[cat])
    },
    openPreferences: function () {
      if (!root) render()
      setOptionsView(true)
    },
    onChange: function (fn) {
      if (typeof fn === 'function') {
        listeners.push(fn)
        var c = readCookie()
        if (c) fn(c)
      }
    },
    reset: function () {
      clearCookie()
      if (!root) render()
    },
  }
  window.MatrixConsent = api

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot)
  } else {
    boot()
  }
})()
