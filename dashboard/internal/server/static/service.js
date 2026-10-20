(function(){
  function $(id){ return document.getElementById(id) }

  // Round to `digits` decimals and drop any trailing zeros, so a whole value
  // reads as '100' rather than '100.00'.
  function trimNum(n, digits){
    const s = n.toFixed(digits)
    if(s.indexOf('.') < 0) return s
    return s.replace(/0+$/, '').replace(/\.$/, '')
  }

  function esc(s){
    return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))
  }

  function formatMs(v){
    if(v == null) return '-'
    if(v >= 10000) return trimNum(v/1000, 1) + ' s'
    return trimNum(v, v < 100 ? 1 : 0) + ' ms'
  }

  const COLORS = ['#58a6ff', '#3fb950', '#d29922', '#bc8cff', '#39c5cf', '#f778ba', '#a5d6ff', '#ffa657']
  const FAIL_COLOR = '#f85149'

  // niceStep picks a 1, 2, 2.5 or 5 times a power of ten step that splits
  // span into about count parts.
  function niceStep(span, count){
    const raw = span / Math.max(1, count)
    const pow = Math.pow(10, Math.floor(Math.log10(raw)))
    for(const m of [1, 2, 2.5, 5]){
      if(raw <= m*pow) return m*pow
    }
    return 10*pow
  }

  const MINUTE = 60000, HOUR = 60*MINUTE, DAY = 24*HOUR
  const TIME_STEPS = [MINUTE, 5*MINUTE, 10*MINUTE, 15*MINUTE, 30*MINUTE, HOUR, 2*HOUR, 3*HOUR, 6*HOUR, 12*HOUR, DAY, 2*DAY, 7*DAY, 14*DAY, 30*DAY]

  // timeTicks returns tick times between from and to, at the smallest step
  // that keeps them to maxTicks, aligned to local clock boundaries. Steps of
  // an hour or more walk the local calendar, so they stay on the hour or on
  // midnight across a daylight saving change.
  function timeTicks(from, to, maxTicks){
    let step = TIME_STEPS[TIME_STEPS.length-1]
    for(const s of TIME_STEPS){
      if((to - from) / s <= maxTicks){ step = s; break }
    }
    const ticks = []
    if(step < HOUR){
      for(let t = Math.ceil(from / step) * step; t <= to; t += step) ticks.push(t)
      return {step, ticks}
    }
    const s = new Date(from)
    const y = s.getFullYear(), mo = s.getMonth(), d = s.getDate()
    let at
    if(step < DAY){
      const hours = step / HOUR
      let h = Math.ceil(s.getHours() / hours) * hours
      if(new Date(y, mo, d, h).getTime() < from) h += hours
      at = k => new Date(y, mo, d, h + k*hours).getTime()
    } else {
      const days = step / DAY
      const d0 = new Date(y, mo, d).getTime() < from ? d + 1 : d
      at = k => new Date(y, mo, d0 + k*days).getTime()
    }
    for(let k = 0, t = at(0); t <= to; t = at(++k)) ticks.push(t)
    return {step, ticks}
  }

  function formatTick(t, step){
    const d = new Date(t)
    if(step >= DAY || (d.getHours() === 0 && d.getMinutes() === 0)){
      return d.toLocaleDateString(undefined, {month: 'short', day: 'numeric'})
    }
    return d.toLocaleTimeString(undefined, {hour: 'numeric', minute: '2-digit'})
  }

  function formatWhen(t){
    return new Date(t).toLocaleString(undefined, {month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit', second: '2-digit'})
  }

  function resizeCanvas(canvas){
    const rect = canvas.getBoundingClientRect()
    const dpr = window.devicePixelRatio || 1
    canvas.width = Math.floor(rect.width * dpr)
    canvas.height = Math.floor(rect.height * dpr)
    const ctx = canvas.getContext('2d')
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    return {w: rect.width, h: rect.height}
  }

  function colorFor(lat, name){
    const names = Object.keys(lat.series).sort()
    return COLORS[names.indexOf(name) % COLORS.length]
  }

  // draw paints the chart and returns the plotted points for hover lookup.
  function draw(canvas, lat, names){
    const ctx = canvas.getContext('2d')
    const {w, h} = resizeCanvas(canvas)
    ctx.clearRect(0, 0, w, h)
    const pad = {left: 64, right: 16, top: 14, bottom: 30}
    const plotW = w - pad.left - pad.right, plotH = h - pad.top - pad.bottom
    const from = lat.from * 1000, to = lat.to * 1000
    const muted = getComputedStyle(document.body).getPropertyValue('--muted').trim() || '#8b949e'
    const border = getComputedStyle(document.body).getPropertyValue('--border').trim() || '#30363d'

    let maxY = 0
    for(const n of names){
      for(const p of lat.series[n]){
        const v = p.max != null ? p.max : p.avg
        if(v != null && v > maxY) maxY = v
      }
    }
    if(maxY <= 0) maxY = 1
    const yStep = niceStep(maxY * 1.05, 4)
    const top = Math.ceil(maxY * 1.05 / yStep) * yStep
    const x = t => pad.left + (t - from) / Math.max(1, to - from) * plotW
    const y = v => pad.top + plotH - v / top * plotH

    ctx.font = '12px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif'
    ctx.lineWidth = 1
    ctx.strokeStyle = border
    ctx.fillStyle = muted
    ctx.textAlign = 'right'
    ctx.textBaseline = 'middle'
    for(let v = 0; v <= top + yStep / 2; v += yStep){
      const yy = Math.round(y(v)) + 0.5
      ctx.beginPath(); ctx.moveTo(pad.left, yy); ctx.lineTo(w - pad.right, yy); ctx.stroke()
      ctx.fillText(formatMs(v), pad.left - 8, yy)
    }

    const {step, ticks} = timeTicks(from, to, Math.max(2, Math.floor(plotW / 90)))
    ctx.textAlign = 'center'
    ctx.textBaseline = 'top'
    for(const t of ticks){
      const xx = Math.round(x(t)) + 0.5
      ctx.beginPath(); ctx.moveTo(xx, pad.top + plotH); ctx.lineTo(xx, pad.top + plotH + 4); ctx.stroke()
      const label = formatTick(t, step)
      const half = ctx.measureText(label).width / 2
      ctx.fillText(label, Math.min(Math.max(xx, pad.left + half), w - pad.right - half), pad.top + plotH + 8)
    }

    const hits = []
    for(const n of names){
      const pts = lat.series[n]
      const color = colorFor(lat, n)
      const spacing = []
      for(let i = 1; i < pts.length; i++) spacing.push(pts[i].t - pts[i-1].t)
      spacing.sort((a, b) => a - b)
      const median = spacing.length ? spacing[Math.floor(spacing.length / 2)] : Infinity
      // A gap breaks the line when it is over three times the usual spacing,
      // or over three times what the earlier point covers.
      const joined = (prev, p) => p.t - prev.t <= 3 * Math.max(median, prev.span || 0)

      if(lat.bucket > 0){
        ctx.strokeStyle = color
        ctx.globalAlpha = 0.25
        for(const p of pts){
          if(p.min == null) continue
          const xx = x(p.t * 1000)
          ctx.beginPath(); ctx.moveTo(xx, y(p.min)); ctx.lineTo(xx, y(p.max)); ctx.stroke()
        }
        ctx.globalAlpha = 1
      }

      ctx.strokeStyle = color
      ctx.lineWidth = 1.5
      ctx.beginPath()
      let prev = null
      for(const p of pts){
        if(p.avg == null){ prev = null; continue }
        const xx = x(p.t * 1000), yy = y(p.avg)
        if(prev && joined(prev, p)) ctx.lineTo(xx, yy); else ctx.moveTo(xx, yy)
        prev = p
      }
      ctx.stroke()
      ctx.lineWidth = 1

      for(const p of pts){
        const xx = x(p.t * 1000)
        if(p.avg != null){
          const yy = y(p.avg)
          ctx.fillStyle = color
          ctx.beginPath(); ctx.arc(xx, yy, 1.8, 0, 2 * Math.PI); ctx.fill()
          hits.push({x: xx, y: yy, p, name: n, color})
        }
        if(p.failed > 0){
          const yy = pad.top + 3
          ctx.fillStyle = FAIL_COLOR
          ctx.beginPath(); ctx.moveTo(xx - 3.5, yy - 3); ctx.lineTo(xx + 3.5, yy - 3); ctx.lineTo(xx, yy + 3); ctx.closePath(); ctx.fill()
          if(p.avg == null) hits.push({x: xx, y: yy, p, name: n, color: FAIL_COLOR})
        }
      }
    }
    return hits
  }

  function tooltip(hit, bucket){
    const p = hit.p
    let html = '<strong>' + esc(formatWhen(p.t * 1000)) + '</strong>' +
      '<span style="color:' + hit.color + '">&#9679;</span> ' + esc(hit.name) + '<br>'
    if(p.avg != null){
      html += (p.checks > 1 ? 'avg ' : '') + esc(formatMs(p.avg))
      if(p.checks > 1 && p.min != null) html += ' (' + esc(formatMs(p.min)) + ' to ' + esc(formatMs(p.max)) + ')'
      html += '<br>'
    }
    if(p.checks > 1 || bucket > 0) html += p.checks + ' check' + (p.checks === 1 ? '' : 's') + '<br>'
    if(p.failed > 0) html += '<span style="color:' + FAIL_COLOR + '">' + p.failed + ' failed</span>'
    return html
  }

  function renderStats(el, lat, names){
    if(names.length === 0){
      el.innerHTML = '<p class="meta">No checks in this range.</p>'
      return
    }
    let html = '<table class="stats"><thead><tr><th>Location</th><th>Avg</th><th>Min</th><th>Max</th><th>Checks</th><th>Failed</th></tr></thead><tbody>'
    for(const n of names){
      const s = lat.stats[n]
      html += '<tr><td>' + esc(n) + '</td><td>' + esc(formatMs(s.avg)) + '</td><td>' + esc(formatMs(s.min)) +
        '</td><td>' + esc(formatMs(s.max)) + '</td><td>' + s.checks + '</td><td>' + s.failed + '</td></tr>'
    }
    el.innerHTML = html + '</tbody></table>'
  }

  function renderLegend(el, lat, names){
    el.innerHTML = names.map(n => '<span><span class="swatch" style="background:' + colorFor(lat, n) + '"></span>' + esc(n) + '</span>').join('') +
      (names.length ? '<span><span class="swatch" style="background:' + FAIL_COLOR + '"></span>failed check</span>' : '')
  }

  document.addEventListener('DOMContentLoaded', ()=>{
    const dash = document.body.dataset.dash
    const svc = document.body.dataset.service
    if(!dash || !svc) return
    const locSel = $('location-select')
    const rangeSel = $('time-frame-select')
    const canvas = $('rt-chart')
    const tip = $('rt-tip')
    let lat = null
    let hits = []
    let requested = 0

    function names(){
      if(!lat) return []
      const all = Object.keys(lat.series).sort()
      return locSel.value === 'all' ? all : all.filter(n => n === locSel.value)
    }

    function render(){
      if(!lat) return
      const shown = names()
      hits = draw(canvas, lat, shown)
      renderLegend($('rt-legend'), lat, shown)
      renderStats($('rt-stats'), lat, shown)
    }

    function load(){
      const url = '/api/dashboard/' + encodeURIComponent(dash) + '/' + encodeURIComponent(svc) + '/latency?range=' + encodeURIComponent(rangeSel.value)
      const request = ++requested
      fetch(url).then(r => {
        if(!r.ok) throw new Error('HTTP ' + r.status)
        return r.json()
      }).then(data => {
        if(request !== requested) return
        lat = data
        const keep = locSel.value
        locSel.length = 1
        for(const n of Object.keys(lat.series).sort()){
          const opt = document.createElement('option'); opt.value = n; opt.textContent = n; locSel.appendChild(opt)
        }
        locSel.value = Array.from(locSel.options).some(o => o.value === keep) ? keep : 'all'
        render()
      }).catch(err => {
        if(request !== requested) return
        console.error('fetch latency failed', err)
        lat = null
        hits = []
        tip.hidden = true
        const {w, h} = resizeCanvas(canvas)
        canvas.getContext('2d').clearRect(0, 0, w, h)
        $('rt-legend').innerHTML = ''
        $('rt-stats').innerHTML = '<p class="meta">Response times are unavailable right now.</p>'
      })
    }

    canvas.addEventListener('mousemove', ev => {
      const rect = canvas.getBoundingClientRect()
      const mx = ev.clientX - rect.left, my = ev.clientY - rect.top
      let best = null, bestD = Infinity
      for(const hit of hits){
        const d = Math.abs(hit.x - mx) * 4 + Math.abs(hit.y - my)
        if(d < bestD){ bestD = d; best = hit }
      }
      if(!best || Math.abs(best.x - mx) > 24){ tip.hidden = true; return }
      tip.innerHTML = tooltip(best, lat.bucket)
      tip.hidden = false
      const left = Math.min(best.x + 12, rect.width - tip.offsetWidth - 4)
      const topPos = Math.max(best.y - tip.offsetHeight - 8, 0)
      tip.style.left = Math.max(left, 0) + 'px'
      tip.style.top = topPos + 'px'
    })
    canvas.addEventListener('mouseleave', () => { tip.hidden = true })
    locSel.addEventListener('change', render)
    rangeSel.addEventListener('change', load)
    window.addEventListener('resize', render)
    load()
  })
})();
