/* Matrix Icons — gallery interactions (v2) */
;(function () {
  'use strict'
  var SVGNS = 'http://www.w3.org/2000/svg'
  var ICONS = window.MATRIX_ICONS || []
  var CATS = window.MATRIX_ICON_CATEGORIES || []

  var root = document.documentElement
  var body = document.body
  var main = document.getElementById('main')
  var q = document.getElementById('q')
  var pills = document.getElementById('pills')
  var shownEl = document.getElementById('shown')
  var totalEl = document.getElementById('total')
  var swEl = document.getElementById('sw')
  var szEl = document.getElementById('sz')

  var activeCat = 'All'
  var query = ''

  totalEl.textContent = ICONS.length

  /* ——— SVG string builders ——————————————————————— */
  function svgMarkup(icon, sw) {
    return (
      '<svg xmlns="' +
      SVGNS +
      '" width="24" height="24" viewBox="0 0 24 24" fill="none" ' +
      'stroke="currentColor" stroke-width="' +
      sw +
      '" stroke-linecap="round" stroke-linejoin="round">\n  ' +
      icon.b.replace(/></g, '>\n  <') +
      '\n</svg>'
    )
  }
  function pascal(name) {
    return name
      .split(/[-_]/)
      .map(function (p) {
        return p.charAt(0).toUpperCase() + p.slice(1)
      })
      .join('')
  }
  function jsxMarkup(icon, sw) {
    var jbody = icon.b.replace(/></g, '>\n    <')
    return (
      'const ' +
      pascal(icon.n) +
      'Icon = (props) => (\n' +
      '  <svg xmlns="' +
      SVGNS +
      '" width={24} height={24} viewBox="0 0 24 24" fill="none"\n' +
      '    stroke="currentColor" strokeWidth={' +
      sw +
      '} strokeLinecap="round" strokeLinejoin="round" {...props}>\n    ' +
      jbody +
      '\n  </svg>\n);'
    )
  }

  /* ——— Inline glyph (for grid/preview) ——————————— */
  function glyph(icon) {
    var s = document.createElementNS(SVGNS, 'svg')
    s.setAttribute('viewBox', '0 0 24 24')
    s.setAttribute('fill', 'none')
    s.setAttribute('stroke', 'currentColor')
    s.setAttribute('stroke-linecap', 'round')
    s.setAttribute('stroke-linejoin', 'round')
    s.style.strokeWidth = 'var(--sw)'
    s.innerHTML = icon.b
    return s
  }

  /* ——— Clipboard + toast ————————————————————————— */
  var toast = document.getElementById('toast')
  var toastMsg = document.getElementById('toast-msg')
  var toastTimer = null
  function ping(label, name) {
    toastMsg.innerHTML = label + (name ? ' <b>' + name + '</b>' : '')
    toast.classList.add('show')
    clearTimeout(toastTimer)
    toastTimer = setTimeout(function () {
      toast.classList.remove('show')
    }, 1900)
  }
  function copy(text, label, name) {
    var done = function () {
      ping(label, name)
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, function () {
        fallbackCopy(text)
        done()
      })
    } else {
      fallbackCopy(text)
      done()
    }
  }
  function fallbackCopy(text) {
    var ta = document.createElement('textarea')
    ta.value = text
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    try {
      document.execCommand('copy')
    } catch (e) {}
    document.body.removeChild(ta)
  }

  /* ——— Render grid ——————————————————————————————— */
  function matches(icon) {
    if (activeCat !== 'All' && icon.c !== activeCat) return false
    if (!query) return true
    var hay = (icon.n + ' ' + icon.c + ' ' + icon.k).toLowerCase()
    return query.split(/\s+/).every(function (t) {
      return hay.indexOf(t) !== -1
    })
  }

  function render() {
    var list = ICONS.filter(matches)
    shownEl.textContent = list.length
    main.innerHTML = ''

    if (!list.length) {
      var e = document.createElement('div')
      e.className = 'empty'
      e.innerHTML = 'No icons match <code>' + (query || activeCat) + '</code>'
      main.appendChild(e)
      return
    }

    // group by category, preserving CATS order
    var order = activeCat === 'All' ? CATS : [activeCat]
    order.forEach(function (cat) {
      var items = list.filter(function (i) {
        return i.c === cat
      })
      if (!items.length) return

      var head = document.createElement('div')
      head.className = 'group-head'
      head.innerHTML =
        '<h2>' + cat + '</h2><span class="line"></span><span class="gc">' + items.length + '</span>'
      main.appendChild(head)

      var grid = document.createElement('div')
      grid.className = 'grid'
      items.forEach(function (icon) {
        var cell = document.createElement('div')
        cell.className = 'cell'
        cell.setAttribute('role', 'button')
        cell.setAttribute('tabindex', '0')
        cell.title = icon.n

        var ico = document.createElement('div')
        ico.className = 'ico'
        ico.appendChild(glyph(icon))

        var nm = document.createElement('div')
        nm.className = 'nm'
        nm.textContent = icon.n

        var quick = document.createElement('button')
        quick.className = 'quick'
        quick.title = 'Copy SVG'
        quick.innerHTML =
          '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><rect x="8.5" y="8.5" width="11" height="11" rx="2"/><path d="M15.5 8.5V6.5a2 2 0 0 0-2-2h-7a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h2"/></svg>'
        quick.addEventListener('click', function (ev) {
          ev.stopPropagation()
          copy(svgMarkup(icon, curWeight()), 'Copied SVG', icon.n)
        })

        cell.appendChild(quick)
        cell.appendChild(ico)
        cell.appendChild(nm)
        cell.addEventListener('click', function () {
          openDrawer(icon)
        })
        cell.addEventListener('keydown', function (ev) {
          if (ev.key === 'Enter' || ev.key === ' ') {
            ev.preventDefault()
            openDrawer(icon)
          }
        })
        grid.appendChild(cell)
      })
      main.appendChild(grid)
    })
  }

  function curWeight() {
    return (
      parseFloat(swEl.value)
        .toFixed(2)
        .replace(/\.?0+$/, '') || '1.75'
    )
  }

  /* ——— Category pills ————————————————————————————— */
  function buildPills() {
    var all = ['All'].concat(CATS)
    all.forEach(function (cat) {
      var b = document.createElement('button')
      b.className = 'pill'
      b.textContent = cat
      b.setAttribute('aria-pressed', cat === 'All' ? 'true' : 'false')
      b.addEventListener('click', function () {
        activeCat = cat
        ;[].forEach.call(pills.children, function (c) {
          c.setAttribute('aria-pressed', c === b ? 'true' : 'false')
        })
        render()
      })
      pills.appendChild(b)
    })
  }

  /* ——— Drawer ————————————————————————————————————— */
  var scrim = document.getElementById('scrim')
  var drawer = document.getElementById('drawer')
  var dCat = document.getElementById('d-cat')
  var dName = document.getElementById('d-name')
  var dKeys = document.getElementById('d-keys')
  var dPv = document.getElementById('d-pv')
  var current = null

  function openDrawer(icon) {
    current = icon
    dCat.textContent = icon.c
    dName.textContent = icon.n
    dPv.innerHTML = ''
    dPv.appendChild(glyph(icon))
    dKeys.innerHTML = ''
    icon.k
      .split(/\s+/)
      .slice(0, 10)
      .forEach(function (k) {
        var t = document.createElement('span')
        t.className = 'dkey'
        t.textContent = k
        dKeys.appendChild(t)
      })
    document.getElementById('cp-name-hint').textContent = icon.n
    scrim.classList.add('show')
    drawer.classList.add('show')
    drawer.setAttribute('aria-hidden', 'false')
  }
  function closeDrawer() {
    scrim.classList.remove('show')
    drawer.classList.remove('show')
    drawer.setAttribute('aria-hidden', 'true')
    current = null
  }
  scrim.addEventListener('click', closeDrawer)
  document.getElementById('d-close').addEventListener('click', closeDrawer)
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') closeDrawer()
  })

  document.getElementById('cp-svg').addEventListener('click', function () {
    if (current) copy(svgMarkup(current, curWeight()), 'Copied SVG', current.n)
  })
  document.getElementById('cp-jsx').addEventListener('click', function () {
    if (current) copy(jsxMarkup(current, curWeight()), 'Copied JSX', pascal(current.n) + 'Icon')
  })
  document.getElementById('cp-name').addEventListener('click', function () {
    if (current) copy(current.n, 'Copied name', current.n)
  })

  /* ——— Controls ——————————————————————————————————— */
  swEl.addEventListener('input', function () {
    root.style.setProperty('--sw', swEl.value)
  })
  szEl.addEventListener('input', function () {
    root.style.setProperty('--isz', szEl.value + 'px')
  })
  document.getElementById('cv-dark').addEventListener('click', function () {
    body.setAttribute('data-canvas', 'dark')
    this.setAttribute('aria-pressed', 'true')
    document.getElementById('cv-light').setAttribute('aria-pressed', 'false')
  })
  document.getElementById('cv-light').addEventListener('click', function () {
    body.setAttribute('data-canvas', 'light')
    this.setAttribute('aria-pressed', 'true')
    document.getElementById('cv-dark').setAttribute('aria-pressed', 'false')
  })

  var qTimer = null
  q.addEventListener('input', function () {
    clearTimeout(qTimer)
    qTimer = setTimeout(function () {
      query = q.value.trim().toLowerCase()
      render()
    }, 90)
  })

  /* ——— Init ——————————————————————————————————————— */
  root.style.setProperty('--sw', swEl.value)
  root.style.setProperty('--isz', szEl.value + 'px')
  buildPills()
  render()
})()
