(function(){
  function $(id){return document.getElementById(id)}
  function fetchData(dash, svc){
    return fetch('/api/dashboard/'+encodeURIComponent(dash)+'/'+encodeURIComponent(svc)).then(r=>r.json())
  }

  function formatPct(s){ return s === '' ? '-' : s }

  function renderSLA(value){
    if(!value) return '—'
    const m = value.match(/^(\d+(?:\.\d+)?)%$/)
    if(m) return '<span class="sla-num">'+m[1]+'</span><span class="sla-percent">%</span>'
    return value
  }

  function slaClass(value){
    const m = value && value.match(/^(\d+(?:\.\d+)?)%$/)
    if(!m) return ''
    const n = parseFloat(m[1])
    if(n >= 99.9) return 'sla-good'
    if(n >= 99.5) return 'sla-warning'
    return 'sla-danger'
  }

  function setSLA(el, value){
    el.innerHTML = renderSLA(value)
    el.classList.remove('sla-good', 'sla-warning', 'sla-danger')
    const cls = slaClass(value)
    if(cls) el.classList.add(cls)
  }

  function computeSLAs(rollups){
    const now = new Date()
    const msPerDay = 24*3600*1000
    const windows = [{label:'7d', days:7},{label:'30d', days:30},{label:'365d', days:365}]
    const out = {}
    // transform rollups map into array of {t, success, failure}
    const hrs = Object.keys(rollups).map(k=>({t: parseInt(k,10)*1000, r: rollups[k]})).sort((a,b)=>a.t-b.t)
    windows.forEach(w=>{
      const from = new Date(now.getTime() - (w.days-1)*msPerDay)
      let succ=0, fail=0
      for(const h of hrs){
        const t = new Date(h.t)
        if(t>=from && t<=now){
          succ += h.r.success || 0
          fail += h.r.failure || 0
        }
      }
      const total = succ+fail
      if(total===0){
        out[w.label] = ''
      } else {
        const pct = succ/total*100
        out[w.label] = (Number.isInteger(pct) ? pct.toString() : pct.toFixed(2)) + '%'
      }
    })
    return out
  }

  function buildLocations(rollups){
    const set = new Set()
    for(const k in rollups){
      const r = rollups[k]
      if(r && r.perLocation){
        Object.keys(r.perLocation).forEach(l=>set.add(l))
      }
    }
    return Array.from(set).sort()
  }

  function filterHours(rollups, from, to){
    const out = []
    for(const k in rollups){
      const t = parseInt(k,10)*1000
      if(t>=from.getTime() && t<=to.getTime()) out.push({t, r: rollups[k]})
    }
    out.sort((a,b)=>a.t-b.t)
    return out
  }

  function formatTimeLabel(ts, span){
    const d = new Date(ts)
    if(span > 2*24*3600*1000) {
      return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
    }
    return d.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })
  }

  function resizeCanvas(canvas){
    const rect = canvas.getBoundingClientRect()
    const dpr = window.devicePixelRatio || 1
    canvas.width = Math.floor(rect.width * dpr)
    canvas.height = Math.floor(rect.height * dpr)
    const ctx = canvas.getContext('2d')
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    return { w: rect.width, h: rect.height }
  }

  function drawChart(canvas, points){
    const ctx = canvas.getContext('2d')
    const { w, h } = resizeCanvas(canvas)
    ctx.clearRect(0,0,w,h)
    if(points.length===0) return
    const valid = points.filter(p=>p.value!=null)
    if(valid.length===0) return
    const xs = points.map(p=>p.t)
    const minX = xs[0], maxX = xs[xs.length-1]
    const vals = valid.map(p=>p.value)
    let minY = Math.min(0, Math.min(...vals)), maxY = Math.max(...vals)
    if(minY === maxY) {
      maxY = minY + 1
    }
    const padding = { left: 50, right: 16, top: 18, bottom: 28 }
    const plotW = w - padding.left - padding.right
    const plotH = h - padding.top - padding.bottom
    const muted = getComputedStyle(document.body).getPropertyValue('--muted') || '#8b949e'
    const border = getComputedStyle(document.body).getPropertyValue('--border') || '#30363d'

    ctx.font = '12px Inter, ui-sans-serif, system-ui, sans-serif'
    ctx.fillStyle = muted
    ctx.strokeStyle = border
    ctx.lineWidth = 1

    const ticks = 5
    for(let i = 0; i < ticks; i++){
      const y = padding.top + plotH - (plotH/(ticks-1))*i
      const value = minY + (maxY-minY)/(ticks-1)*i
      ctx.beginPath(); ctx.moveTo(padding.left, y); ctx.lineTo(w-padding.right, y); ctx.stroke()
      const label = Math.round(value) === value ? value.toString() : value.toFixed(1)
      ctx.fillText(label + ' ms', 4, y + 4)
    }

    const span = maxX - minX
    const xLabels = [0, 0.5, 1].map(r => ({
      t: minX + r * span,
      x: padding.left + r * plotW
    }))
    xLabels.forEach(lbl => {
      ctx.fillText(formatTimeLabel(lbl.t, span), lbl.x - 16, h - 6)
    })

    ctx.strokeStyle = '#0b76ef'
    ctx.lineWidth = 2
    ctx.beginPath()
    points.forEach((p,i)=>{
      const x = padding.left + ((p.t - minX)/(maxX-minX || 1))*plotW
      const y = padding.top + plotH - ((p.value - minY)/(maxY-minY || 1))*plotH
      if(p.value==null){ ctx.moveTo(x,y); return }
      if(i===0 || points[i-1].value==null) ctx.moveTo(x,y); else ctx.lineTo(x,y)
    })
    ctx.stroke()
  }

  function computeStatsPerLocation(hours, location){
    // returns map location -> {avg,min,max}
    const locs = {}
    for(const h of hours){
      const per = h.r.perLocation || {}
      for(const loc in per){
        const lr = per[loc]
        if(!lr || !lr.success) continue
        const val = lr.avgMs || null
        if(val==null) continue
        if(!locs[loc]) locs[loc] = {sum:0, count:0, min: val, max: val}
        locs[loc].sum += val * (lr.success || 1)
        locs[loc].count += (lr.success || 1)
        if(val < locs[loc].min) locs[loc].min = val
        if(val > locs[loc].max) locs[loc].max = val
      }
    }
    // finalize
    const out = {}
    for(const loc in locs){
      const s = locs[loc]
      out[loc] = {avg: s.count? (s.sum/s.count):null, min: s.min, max: s.max}
    }
    return out
  }

  function buildPoints(hours, location){
    // hourly points; when location === 'all' compute average across locations weighted by successes
    const pts = []
    for(const h of hours){
      let value = null
      if(location==='all'){
        let sum=0, cnt=0
        for(const loc in h.r.perLocation||{}){
          const lr = h.r.perLocation[loc]
          if(!lr || !lr.success || !lr.avgMs) continue
          sum += lr.avgMs * (lr.success||1)
          cnt += (lr.success||1)
        }
        if(cnt>0) value = sum/cnt
      } else {
        const lr = (h.r.perLocation||{})[location]
        if(lr && lr.success && lr.avgMs) value = lr.avgMs
      }
      pts.push({t: h.t, value})
    }
    return pts
  }

  document.addEventListener('DOMContentLoaded', ()=>{
    const dash = document.body.dataset.dash
    const svc = document.body.dataset.service
    if(!dash || !svc) return
    const sla7El = $('sla7'), sla30El = $('sla30'), sla365El = $('sla365')
    const locSel = $('location-select')
    const canvas = $('rt-chart')
    const statsEl = $('rt-stats')
    const rangeBtns = document.querySelectorAll('#range-buttons button')

    function renderSLA(value){
      if(!value) return '—'
      const m = value.match(/^(\d+(?:\.\d+)?)%$/)
      if(m) return '<span class="sla-num">'+m[1]+'</span><span class="sla-percent">%</span>'
      return value
    }

    fetchData(dash, svc).then(data=>{
      const rollups = data.rollups || {}
      // populate SLAs
      const slas = computeSLAs(rollups)
      setSLA(sla7El, slas['7d'])
      setSLA(sla30El, slas['30d'])
      setSLA(sla365El, slas['365d'])

      // populate locations
      const locs = buildLocations(rollups)
      for(const l of locs){
        const opt = document.createElement('option'); opt.value = l; opt.textContent = l; locSel.appendChild(opt)
      }

      // default range: previous day
      function chooseRange(range){
        const now = new Date()
        let from, to
        if(range==='1d'){
          // previous day
          const yesterday = new Date(now.getTime() - 24*3600*1000)
          from = new Date(yesterday.getFullYear(), yesterday.getMonth(), yesterday.getDate(), 0,0,0)
          to = new Date(from.getTime() + 24*3600*1000 - 1000)
        } else if(range==='7d'){
          from = new Date(now.getTime() - 7*24*3600*1000)
          to = now
        } else if(range==='30d'){
          from = new Date(now.getTime() - 30*24*3600*1000)
          to = now
        } else if(range==='60d'){
          from = new Date(now.getTime() - 60*24*3600*1000)
          to = now
        } else if(range==='90d'){
          from = new Date(now.getTime() - 90*24*3600*1000)
          to = now
        }
        const hours = filterHours(rollups, from, to)
        const loc = locSel.value || 'all'
        const pts = buildPoints(hours, loc)
        drawChart(canvas, pts)
        // compute and render stats per location
        const stats = computeStatsPerLocation(hours)
        // render table
        let html = '<strong>Stats (Avg / Min / Max ms)</strong><br>'
        const keys = Object.keys(stats).sort()
        if(keys.length===0) html += '<div class="meta">No success latency samples in range.</div>'
        else{
          html += '<table>'
          for(const k of keys){
            const v = stats[k]
            html += '<tr><td>'+k+'</td><td>'+(v.avg? v.avg.toFixed(1):'-')+'</td><td>'+(v.min? v.min.toFixed(1):'-')+'</td><td>'+(v.max? v.max.toFixed(1):'-')+'</td></tr>'
          }
          html += '</table>'
        }
        statsEl.innerHTML = html
      }

      // initial render
      chooseRange('1d')

      // wire controls
      const timeFrameSel = $('time-frame-select')
      locSel.addEventListener('change', ()=> chooseRange(timeFrameSel.value || '1d'))
      timeFrameSel.addEventListener('change', ()=> chooseRange(timeFrameSel.value || '1d'))
    }).catch(err=>{
      console.error('fetch rollups failed', err)
    })
  })
})();
