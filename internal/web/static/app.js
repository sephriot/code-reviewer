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
  connectSSE();
});

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

  const toast = document.createElement('div');
  toast.className = 'toast toast-' + event.Type;
  toast.textContent = msg;
  document.body.appendChild(toast);
  setTimeout(function() { toast.remove(); }, 5000);
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
