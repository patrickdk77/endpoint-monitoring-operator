(function(){
  function $(id){return document.getElementById(id)}
  function fetchData(dash, svc){
    return fetch('/api/dashboard/'+encodeURIComponent(dash)+'/'+encodeURIComponent(svc)).then(r=>r.json())
  }

  function formatPct(s){ return s === '' ? '-' : s }

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
          succ += h.r.Success || 0
          fail += h.r.Failure || 0
        }
      }
      const total = succ+fail
      out[w.label] = total===0 ? '' : ((succ/total*100).toFixed(2)+'%')
    })
    return out
  }

  function buildLocations(rollups){
    const set = new Set()
    for(const k in rollups){
      const r = rollups[k]
      if(r && r.PerLocation){
        Object.keys(r.PerLocation).forEach(l=>set.add(l))
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

  function drawChart(canvas, points){
    const ctx = canvas.getContext('2d')
    const w = canvas.width, h = canvas.height
    ctx.clearRect(0,0,w,h)
    if(points.length===0) return
    // compute ranges, ignore nulls
    const valid = points.filter(p=>p.value!=null)
    if(valid.length===0) return
    const xs = points.map(p=>p.t)
    const minX = xs[0], maxX = xs[xs.length-1]
    const vals = valid.map(p=>p.value)
    const minY = Math.min(...vals), maxY = Math.max(...vals)
    const pad = 10
    // axes
    ctx.strokeStyle = '#ccc'; ctx.lineWidth=1
    ctx.beginPath(); ctx.moveTo(pad,h-pad); ctx.lineTo(w-pad,h-pad); ctx.stroke();
    ctx.beginPath(); ctx.moveTo(pad,pad); ctx.lineTo(pad,h-pad); ctx.stroke();
    // draw polyline
    ctx.strokeStyle = '#0b76ef'; ctx.lineWidth=2; ctx.beginPath()
    points.forEach((p,i)=>{
      const x = pad + ((p.t - minX)/(maxX-minX || 1))*(w-2*pad)
      const y = h-pad - ((p.value - minY)/(maxY-minY || 1))*(h-2*pad)
      if(p.value==null){ ctx.moveTo(x,y); return }
      if(i===0 || points[i-1].value==null) ctx.moveTo(x,y); else ctx.lineTo(x,y)
    })
    ctx.stroke()
  }

  function computeStatsPerLocation(hours, location){
    // returns map location -> {avg,min,max}
    const locs = {}
    for(const h of hours){
      const per = h.r.PerLocation || {}
      for(const loc in per){
        const lr = per[loc]
        if(!lr || !lr.Success) continue
        const val = lr.AvgMs || null
        if(val==null) continue
        if(!locs[loc]) locs[loc] = {sum:0, count:0, min: val, max: val}
        locs[loc].sum += val * (lr.Success || 1)
        locs[loc].count += (lr.Success || 1)
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
        for(const loc in h.r.PerLocation||{}){
          const lr = h.r.PerLocation[loc]
          if(!lr || !lr.Success || !lr.AvgMs) continue
          sum += lr.AvgMs * (lr.Success||1)
          cnt += (lr.Success||1)
        }
        if(cnt>0) value = sum/cnt
      } else {
        const lr = (h.r.PerLocation||{})[location]
        if(lr && lr.Success && lr.AvgMs) value = lr.AvgMs
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

    fetchData(dash, svc).then(data=>{
      const rollups = data.rollups || {}
      // populate SLAs
      const slas = computeSLAs(rollups)
      sla7El.textContent = slas['7d'] || '-'
      sla30El.textContent = slas['30d'] || '-'
      sla365El.textContent = slas['365d'] || '-'

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
      locSel.addEventListener('change', ()=> chooseRange(document.querySelector('#range-buttons button.active')?.dataset.range || '1d'))
      rangeBtns.forEach(b=>{
        b.addEventListener('click', ()=>{
          rangeBtns.forEach(x=>x.classList.remove('active'))
          b.classList.add('active')
          chooseRange(b.dataset.range)
        })
      })
      // default active button
      document.querySelector('#range-buttons button[data-range="1d"]').classList.add('active')
    }).catch(err=>{
      console.error('fetch rollups failed', err)
    })
  })
})();
