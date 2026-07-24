document.addEventListener('DOMContentLoaded', function() {
  if (Notification && Notification.permission === 'default') {
    Notification.requestPermission();
  }

  const evtSource = new EventSource('/events');
  evtSource.onmessage = function(e) {
    try {
      const event = JSON.parse(e.data);
      handleEvent(event);
    } catch (err) {
      console.error('SSE parse error', err);
    }
  };
});

function handleEvent(event) {
  const msg = event.Message || event.Type;
  const title = 'Code Reviewer';

  if (Notification && Notification.permission === 'granted') {
    new Notification(title, { body: msg });
  }

  const toast = document.createElement('div');
  toast.className = 'toast toast-' + event.Type;
  toast.textContent = msg;
  document.body.appendChild(toast);
  setTimeout(function() { toast.remove(); }, 5000);
}
