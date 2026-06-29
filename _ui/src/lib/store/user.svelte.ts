import axios from 'axios';

// X-User store — kutu has no login, but the backend stamps an audit
// actor from the `X-User` request header (see internal/server/server.go
// actorMiddleware → storage updated_by). This store lets the operator
// pick the name they want their mutations attributed to. The value is
// purely local (per-browser, localStorage) and is applied as an axios
// default header so every subsequent request carries it.

const LOCAL_X_USER_KEY = 'kutu.x_user';

function readLocal(): string {
  if (typeof window === 'undefined') return '';
  try {
    return window.localStorage.getItem(LOCAL_X_USER_KEY) ?? '';
  } catch {
    // localStorage can throw in private mode / disabled storage.
    return '';
  }
}

function writeLocal(value: string): void {
  if (typeof window === 'undefined') return;
  try {
    if (value) window.localStorage.setItem(LOCAL_X_USER_KEY, value);
    else window.localStorage.removeItem(LOCAL_X_USER_KEY);
  } catch {
    // ignore quota / private-mode errors — in-memory state still works.
  }
}

// applyHeader installs (or clears) the X-User default on axios so it
// rides along with every request without per-call plumbing.
function applyHeader(value: string): void {
  if (value) {
    axios.defaults.headers.common['X-User'] = value;
  } else {
    delete axios.defaults.headers.common['X-User'];
  }
}

function createUserStore() {
  const initial = readLocal();
  let xUser = $state<string>(initial);

  // Apply synchronously at module init so the header is present before
  // the first mutation fires.
  applyHeader(initial);

  function setUser(value: string): void {
    xUser = value.trim();
    writeLocal(xUser);
    applyHeader(xUser);
  }

  return {
    get user() {
      return xUser;
    },
    setUser,
  };
}

export const userStore = createUserStore();
