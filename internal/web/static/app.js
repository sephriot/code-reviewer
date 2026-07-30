var evtSource;

function connectSSE() {
  if (evtSource) {
    evtSource.close();
  }
  evtSource = new EventSource('/events');
  evtSource.onmessage = function(e) {
    try {
      const event = JSON.parse(e.data);
      handleEvent(event);
    } catch (err) {
      console.error('SSE parse error', err);
    }
  };
  evtSource.onerror = function() {
    evtSource.close();
  };
}

function disconnectSSE() {
  if (evtSource) {
    evtSource.close();
    evtSource = null;
  }
}

window.addEventListener('beforeunload', disconnectSSE);

document.addEventListener('DOMContentLoaded', function() {
  if (Notification && Notification.permission === 'default') {
    Notification.requestPermission();
  }
  consumeFlashToast();
  connectSSE();
  initMuteToggle();
  initActiveNav();
});

function initActiveNav() {
  const path = window.location.pathname || '/';
  document.querySelectorAll('.nav-links a[data-nav]').forEach(function(a) {
    const nav = a.getAttribute('data-nav');
    const active = nav === '/' ? path === '/' : path === nav || path.indexOf(nav + '/') === 0;
    a.classList.toggle('active', active);
  });
}

function initMuteToggle() {
  const el = document.getElementById('mute-notifications');
  const wrap = document.getElementById('mute-toggle');
  if (!el || !wrap) return;

  function applyMuteUI(muted) {
    el.checked = muted;
    wrap.classList.toggle('is-muted', muted);
    const label = wrap.querySelector('.mute-toggle-label');
    if (label) {
      label.textContent = muted ? (label.dataset.off || 'Muted') : (label.dataset.on || 'Sound on');
    }
  }

  fetch('/api/notifications/mute')
    .then(function(res) { return res.ok ? res.json() : null; })
    .then(function(data) {
      if (data && typeof data.muted === 'boolean') {
        applyMuteUI(data.muted);
      }
    })
    .catch(function() {});

  el.addEventListener('change', function() {
    const muted = el.checked;
    applyMuteUI(muted);
    fetch('/api/notifications/mute', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ muted: muted })
    }).then(function(res) {
      if (!res.ok) {
        applyMuteUI(!muted);
      }
    }).catch(function() {
      applyMuteUI(!muted);
    });
  });
}

function showToast(message, type) {
  const toast = document.createElement('div');
  toast.className = 'toast toast-' + (type || 'info');
  toast.textContent = message;
  document.body.appendChild(toast);
  setTimeout(function() { toast.remove(); }, 5000);
}

function flashToast(message, type) {
  try {
    sessionStorage.setItem('cr-toast', JSON.stringify({
      message: message,
      type: type || 'info'
    }));
  } catch (e) {}
}

function consumeFlashToast() {
  try {
    const raw = sessionStorage.getItem('cr-toast');
    if (!raw) return;
    sessionStorage.removeItem('cr-toast');
    const data = JSON.parse(raw);
    if (data && data.message) {
      showToast(data.message, data.type);
    }
  } catch (e) {}
}

async function runAction(url, opts) {
  opts = opts || {};
  try {
    const res = await fetch(url, {
      method: opts.method || 'POST',
      headers: opts.headers,
      body: opts.body
    });
    if (!res.ok) {
      const body = (await res.text()).trim();
      showToast(body || ('Request failed (' + res.status + ')'), 'error');
      return false;
    }
    if (opts.successMessage) {
      if (opts.reload || opts.redirect) {
        flashToast(opts.successMessage, 'success');
      } else {
        showToast(opts.successMessage, 'success');
      }
    }
    if (opts.reload) {
      location.reload();
    } else if (opts.redirect) {
      location.href = opts.redirect;
    }
    return true;
  } catch (err) {
    showToast(String(err.message || err), 'error');
    return false;
  }
}

function handleEvent(event) {
  const msg = event.Message || event.Type;
  const title = 'Code Reviewer';

  if (Notification && Notification.permission === 'granted') {
    const n = new Notification(title, { body: msg });
    const prURL = event.PR ? '/pr/' + event.PR.id : null;
    if (prURL) {
      n.onclick = function() { window.open(prURL, '_blank'); this.close(); };
    }
  }

  showToast(msg, event.Type);
}

async function removeQueueItem(id, btn) {
  if (btn) btn.disabled = true;
  try {
    const res = await fetch('/api/review-request/' + id, { method: 'DELETE' });
    if (!res.ok) {
      const body = await res.text();
      alert('Failed to remove queue item: ' + (body || res.status));
      if (btn) btn.disabled = false;
      return;
    }
    const li = btn && btn.closest ? btn.closest('li') : null;
    if (li) li.remove();
  } catch (err) {
    alert('Failed to remove queue item: ' + err);
    if (btn) btn.disabled = false;
  }
}
