// Point this at your relayer's API server.
//
// When testing locally, this app is served on one port (e.g. 5173 via
// `python3 -m http.server`) and the Go API runs on a DIFFERENT port
// (default 8080, from `go run ./cmd/api`) — they are never the same
// origin, so we cannot just reuse window.location.origin here.
//
// Change API_LOCAL_PORT if your API's API_ADDR/PORT differs from 8080.
// Change PRODUCTION_API_URL before deploying this app anywhere real.
const API_LOCAL_PORT = 8080;
const PRODUCTION_API_URL = 'https://your-api-domain.example.com'; // <-- CHANGE THIS for production

// Treat anything that isn't a real public domain as "local dev": localhost,
// 127.0.0.1, 0.0.0.0, and private LAN ranges (192.168.x.x, 10.x.x.x,
// 172.16-31.x.x — for testing from a phone on the same WiFi). This used to
// only check for 'localhost'/'127.0.0.1' and silently fell through to the
// fake PRODUCTION_API_URL placeholder for anything else (e.g. 0.0.0.0),
// causing a confusing ERR_NAME_NOT_RESOLVED with no obvious cause.
function isLocalDevHost(hostname) {
  if (hostname === 'localhost' || hostname === '127.0.0.1' || hostname === '0.0.0.0') return true;
  if (/^192\.168\.\d{1,3}\.\d{1,3}$/.test(hostname)) return true;
  if (/^10\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(hostname)) return true;
  if (/^172\.(1[6-9]|2\d|3[0-1])\.\d{1,3}\.\d{1,3}$/.test(hostname)) return true;
  return false;
}

const API_BASE = isLocalDevHost(location.hostname)
  ? `${location.protocol}//${location.hostname}:${API_LOCAL_PORT}`
  : PRODUCTION_API_URL;

console.log('[BDT Wallet] API_BASE =', API_BASE, '— if requests fail, check this URL is actually running your API.');

let session = {
  token: localStorage.getItem('bdt_token') || null,
  userId: localStorage.getItem('bdt_user_id') || null,
  name: localStorage.getItem('bdt_name') || '',
  phone: localStorage.getItem('bdt_phone') || '',
};

let feeConfig = { internal_transfer_fee: '3', withdraw_fee: '10' };

// --- API helper ---
async function api(path, { method = 'GET', body, auth = false, idempotencyKey = '' } = {}) {
  const headers = { 'Content-Type': 'application/json' };
  if (auth && session.token) headers['Authorization'] = 'Bearer ' + session.token;
  if (idempotencyKey) headers['Idempotency-Key'] = idempotencyKey;

  const res = await fetch(API_BASE + path, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined,
  });
  let data = {};
  try { data = await res.json(); } catch (e) {}
  if (!res.ok) {
    throw new Error(data.error || `request failed (${res.status})`);
  }
  return data;
}

// --- Screen navigation ---
function showScreen(name) {
  document.querySelectorAll('.screen').forEach((el) => el.classList.remove('active'));
  document.getElementById('screen-' + name).classList.add('active');
  const nav = document.getElementById('bottom-nav');
  if (nav) nav.style.display = (name === 'dashboard') ? 'flex' : 'none';
}

function openSheet(name) {
  document.getElementById('sheet-' + name).classList.add('show');
  if (name === 'receive') loadDepositAddress();
}
function closeSheet(name) {
  document.getElementById('sheet-' + name).classList.remove('show');
}

// --- Auth ---
async function doRegister() {
  const btn = document.getElementById('register-btn');
  const errEl = document.getElementById('register-error');
  errEl.classList.remove('show');

  const name = document.getElementById('reg-name').value.trim();
  const phone = document.getElementById('reg-phone').value.trim();
  const pin = document.getElementById('reg-pin').value.trim();

  if (!phone || !/^\+?[0-9]{10,15}$/.test(phone)) {
    errEl.textContent = 'সঠিক ফোন নম্বর দাও';
    errEl.classList.add('show');
    return;
  }
  if (!/^[0-9]{4,8}$/.test(pin)) {
    errEl.textContent = 'PIN ৪-৮ ডিজিটের হতে হবে';
    errEl.classList.add('show');
    return;
  }

  btn.disabled = true;
  btn.textContent = 'অপেক্ষা করো...';
  try {
    const data = await api('/api/v1/mobile/register', {
      method: 'POST',
      body: { phone, pin, display_name: name },
    });
    persistSession(data.token, data.user_id, name, phone);
    await enterDashboard();
  } catch (e) {
    errEl.textContent = e.message;
    errEl.classList.add('show');
  } finally {
    btn.disabled = false;
    btn.textContent = 'রেজিস্টার করো';
  }
}

async function doLogin() {
  const btn = document.getElementById('login-btn');
  const errEl = document.getElementById('login-error');
  errEl.classList.remove('show');

  const phone = document.getElementById('login-phone').value.trim();
  const pin = document.getElementById('login-pin').value.trim();

  btn.disabled = true;
  btn.textContent = 'অপেক্ষা করো...';
  try {
    const data = await api('/api/v1/mobile/login', { method: 'POST', body: { phone, pin } });
    persistSession(data.token, data.user_id, '', phone);
    await enterDashboard();
  } catch (e) {
    errEl.textContent = e.message;
    errEl.classList.add('show');
  } finally {
    btn.disabled = false;
    btn.textContent = 'লগইন করো';
  }
}

function persistSession(token, userId, name, phone) {
  session = { token, userId, name, phone: phone || '' };
  localStorage.setItem('bdt_token', token);
  localStorage.setItem('bdt_user_id', userId);
  localStorage.setItem('bdt_name', name);
  localStorage.setItem('bdt_phone', phone || '');
}

function logout() {
  localStorage.removeItem('bdt_token');
  localStorage.removeItem('bdt_user_id');
  localStorage.removeItem('bdt_name');
  session = { token: null, userId: null, name: '' };
  showScreen('login');
}

// --- Dashboard ---
async function enterDashboard() {
  showScreen('dashboard');
  document.getElementById('user-name').textContent = session.name || ('User #' + session.userId);
  await Promise.all([loadConfig(), loadBalance(), loadHistory()]);
}

async function loadConfig() {
  try {
    const cfg = await api('/api/v1/mobile/config');
    feeConfig = cfg;
    document.getElementById('send-fee-label').textContent = cfg.internal_transfer_fee;
    document.getElementById('withdraw-fee-label').textContent = cfg.withdraw_fee;
    const network = document.getElementById('network-status');
    if (network) network.textContent = cfg.network || 'BDT Wallet';
  } catch (e) { /* non-fatal, defaults already shown */ }
}

async function loadBalance() {
  try {
    const data = await api('/api/v1/mobile/me/balance', { auth: true });
    document.getElementById('balance-amount').innerHTML =
      formatAmount(data.balance) + ' <span>BDT</span>';
  } catch (e) {
    if (String(e.message).includes('session')) return logout();
  }
}

async function loadHistory() {
  const el = document.getElementById('history-list');
  try {
    const entries = await api('/api/v1/mobile/me/history', { auth: true });
    if (!entries || entries.length === 0) {
      el.innerHTML = '<div class="empty-state">এখনো কোনো লেনদেন নেই</div>';
      return;
    }
    el.innerHTML = entries.map((e) => {
      const positive = parseFloat(e.amount) >= 0;
      const label = { transfer: 'Transfer', deposit: 'Deposit', withdraw: 'Withdraw', fee: 'Fee' }[e.type] || e.type;
      const date = new Date(e.created_at).toLocaleString('bn-BD', { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });
      return `<div class="history-item">
        <div><div>${label}</div><div class="meta">${date}</div></div>
        <div class="amt ${positive ? 'pos' : 'neg'}">${positive ? '+' : ''}${formatAmount(e.amount)}</div>
      </div>`;
    }).join('');
  } catch (e) {
    el.innerHTML = '<div class="empty-state">লোড করা যায়নি</div>';
  }
}

function formatAmount(v) {
  const n = parseFloat(v);
  return isNaN(n) ? v : n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

// --- Receive ---
async function loadDepositAddress() {
  const box = document.getElementById('deposit-address');
  const qrEl = document.getElementById('qr-code');
  qrEl.innerHTML = '';
  box.textContent = 'লোড হচ্ছে...';
  try {
    const data = await api('/api/v1/mobile/me/deposit-address', { auth: true });
    box.textContent = data.deposit_address;
    new QRCode(qrEl, {
      text: data.deposit_address,
      width: 180, height: 180,
      colorDark: '#0a0e1a', colorLight: '#ffffff',
    });
  } catch (e) {
    box.textContent = 'Address লোড করা যায়নি: ' + e.message;
  }
}

function copyDepositAddress() {
  const text = document.getElementById('deposit-address').textContent;
  navigator.clipboard.writeText(text).then(() => alert('Address কপি হয়েছে'));
}

// --- Send ---
async function doSend() {
  const btn = document.getElementById('send-btn');
  const resultEl = document.getElementById('send-result');
  resultEl.className = 'result-msg';

  const to_phone = document.getElementById('send-phone').value.trim();
  const amount = document.getElementById('send-amount').value.trim();

  if (!to_phone || !amount || parseFloat(amount) <= 0) {
    resultEl.textContent = 'ফোন নম্বর ও amount দাও';
    resultEl.className = 'result-msg err';
    return;
  }

  btn.disabled = true;
  btn.innerHTML = '<span class="spinner"></span> পাঠাচ্ছে...';
  try {
    await api('/api/v1/mobile/transfer', { method: 'POST', auth: true, body: { to_phone, amount } });
    resultEl.textContent = 'সফলভাবে পাঠানো হয়েছে!';
    resultEl.className = 'result-msg ok';
    document.getElementById('send-phone').value = '';
    document.getElementById('send-amount').value = '';
    await Promise.all([loadBalance(), loadHistory()]);
  } catch (e) {
    resultEl.textContent = e.message;
    resultEl.className = 'result-msg err';
  } finally {
    btn.disabled = false;
    btn.textContent = 'পাঠাও';
  }
}

// --- Withdraw ---
async function doWithdraw() {
  const btn = document.getElementById('withdraw-btn');
  const resultEl = document.getElementById('withdraw-result');
  resultEl.className = 'result-msg';

  const to_address = document.getElementById('withdraw-address').value.trim();
  const amount = document.getElementById('withdraw-amount').value.trim();

  if (!/^0x[a-fA-F0-9]{40}$/.test(to_address)) {
    resultEl.textContent = 'সঠিক BSC address দাও (0x...)';
    resultEl.className = 'result-msg err';
    return;
  }
  if (!amount || parseFloat(amount) <= 0) {
    resultEl.textContent = 'সঠিক amount দাও';
    resultEl.className = 'result-msg err';
    return;
  }

  btn.disabled = true;
  btn.innerHTML = '<span class="spinner"></span> পাঠাচ্ছে...';
  try {
    const idempotencyKey = (crypto.randomUUID ? crypto.randomUUID() : ('wd-' + Date.now() + '-' + Math.random().toString(36).slice(2))).replace(/[^A-Za-z0-9_-]/g, '');
    await api('/api/v1/mobile/withdraw', { method: 'POST', auth: true, body: { to_address, amount }, idempotencyKey });
    resultEl.textContent = 'Withdraw request গৃহীত হয়েছে, শীঘ্রই process হবে।';
    resultEl.className = 'result-msg ok';
    document.getElementById('withdraw-address').value = '';
    document.getElementById('withdraw-amount').value = '';
    await Promise.all([loadBalance(), loadHistory()]);
  } catch (e) {
    resultEl.textContent = e.message;
    resultEl.className = 'result-msg err';
  } finally {
    btn.disabled = false;
    btn.textContent = 'Withdraw Request পাঠাও';
  }
}

// --- Boot ---
(function boot() {
  if (session.token && session.userId) {
    enterDashboard();
  } else {
    showScreen('login');
  }
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('service-worker.js').catch(() => {});
  }
})();

// ============================================================
// Passkey (WebAuthn) — entirely optional. If the browser or server doesn't
// support it, these functions just fail quietly and PIN login (above) keeps
// working exactly as before. Nothing here can lock anyone out of their
// account — a passkey is an ADDITIONAL way in, never the only way.
// ============================================================

function base64urlToBuffer(b64url) {
  const pad = '='.repeat((4 - (b64url.length % 4)) % 4);
  const base64 = (b64url + pad).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(base64);
  const buf = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) buf[i] = raw.charCodeAt(i);
  return buf.buffer;
}

function bufferToBase64url(buf) {
  const bytes = new Uint8Array(buf);
  let str = '';
  for (let i = 0; i < bytes.length; i++) str += String.fromCharCode(bytes[i]);
  return btoa(str).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function passkeysSupported() {
  return !!(window.PublicKeyCredential && navigator.credentials);
}

let lastCheckedPhone = '';
async function checkPasskeyAvailable() {
  if (!passkeysSupported()) return;
  const phone = document.getElementById('login-phone').value.trim();
  const btn = document.getElementById('passkey-login-btn');
  if (!/^\+?[0-9]{10,15}$/.test(phone)) {
    btn.style.display = 'none';
    return;
  }
  if (phone === lastCheckedPhone) return;
  lastCheckedPhone = phone;
  try {
    const data = await api('/api/v1/mobile/passkey/available?phone=' + encodeURIComponent(phone));
    btn.style.display = data.available ? 'block' : 'none';
  } catch (e) {
    btn.style.display = 'none'; // passkeys not configured on this server — fine, PIN still works
  }
}

async function doPasskeyLogin() {
  const phone = document.getElementById('login-phone').value.trim();
  const errEl = document.getElementById('login-error');
  errEl.classList.remove('show');

  try {
    const options = await api('/api/v1/mobile/passkey/login/begin', {
      method: 'POST', body: { phone },
    });
    const publicKey = options.publicKey;
    publicKey.challenge = base64urlToBuffer(publicKey.challenge);
    if (publicKey.allowCredentials) {
      publicKey.allowCredentials = publicKey.allowCredentials.map((c) => ({
        ...c, id: base64urlToBuffer(c.id),
      }));
    }

    const assertion = await navigator.credentials.get({ publicKey });

    const body = {
      id: assertion.id,
      rawId: bufferToBase64url(assertion.rawId),
      type: assertion.type,
      response: {
        clientDataJSON: bufferToBase64url(assertion.response.clientDataJSON),
        authenticatorData: bufferToBase64url(assertion.response.authenticatorData),
        signature: bufferToBase64url(assertion.response.signature),
        userHandle: assertion.response.userHandle ? bufferToBase64url(assertion.response.userHandle) : null,
      },
    };

    const data = await api('/api/v1/mobile/passkey/login/finish?phone=' + encodeURIComponent(phone), {
      method: 'POST', body,
    });
    persistSession(data.token, data.user_id, '', phone);
    await enterDashboard();
  } catch (e) {
    errEl.textContent = 'Passkey লগইন ব্যর্থ হয়েছে: ' + e.message;
    errEl.classList.add('show');
  }
}

async function setupPasskey() {
  const resultEl = document.getElementById('passkey-setup-result');
  resultEl.className = 'result-msg';
  if (!passkeysSupported()) {
    resultEl.textContent = 'এই browser/device passkey সাপোর্ট করে না';
    resultEl.className = 'result-msg err';
    return;
  }

  try {
    const options = await api('/api/v1/mobile/passkey/register/begin', { method: 'POST', auth: true });
    const publicKey = options.publicKey;
    publicKey.challenge = base64urlToBuffer(publicKey.challenge);
    publicKey.user.id = base64urlToBuffer(publicKey.user.id);
    if (publicKey.excludeCredentials) {
      publicKey.excludeCredentials = publicKey.excludeCredentials.map((c) => ({
        ...c, id: base64urlToBuffer(c.id),
      }));
    }

    const credential = await navigator.credentials.create({ publicKey });

    const body = {
      id: credential.id,
      rawId: bufferToBase64url(credential.rawId),
      type: credential.type,
      response: {
        clientDataJSON: bufferToBase64url(credential.response.clientDataJSON),
        attestationObject: bufferToBase64url(credential.response.attestationObject),
      },
    };

    const deviceLabel = encodeURIComponent(navigator.platform || 'Device');
    await api('/api/v1/mobile/passkey/register/finish?label=' + deviceLabel, {
      method: 'POST', auth: true, body,
    });

    resultEl.textContent = 'Passkey সফলভাবে যোগ হয়েছে! এখন থেকে PIN ছাড়াই লগইন করতে পারবে।';
    resultEl.className = 'result-msg ok';
  } catch (e) {
    resultEl.textContent = 'Passkey setup ব্যর্থ হয়েছে: ' + e.message;
    resultEl.className = 'result-msg err';
  }
}

// ============================================================
// Phone verification (inbound SMS) — optional, additive, does not gate
// login or transfers. Just sets a "verified" flag for future use (e.g.
// higher withdrawal limits later, if you choose to add that).
// ============================================================

let verifyPollTimer = null;

async function startPhoneVerify() {
  const resultEl = document.getElementById('verify-phone-result');
  resultEl.className = 'result-msg';
  const startBtn = document.getElementById('verify-phone-start-btn');
  startBtn.disabled = true;

  try {
    const data = await api('/api/v1/mobile/verify-phone/start', {
      method: 'POST', auth: true, body: { phone: session.phone || document.getElementById('login-phone').value },
    });
    document.getElementById('verify-phone-code').textContent = data.code;
    document.getElementById('verify-phone-target').textContent = data.target_number;
    document.getElementById('verify-phone-idle').style.display = 'none';
    document.getElementById('verify-phone-active').style.display = 'block';
    pollVerifyStatus();
  } catch (e) {
    resultEl.textContent = e.message;
    resultEl.className = 'result-msg err';
  } finally {
    startBtn.disabled = false;
  }
}

async function pollVerifyStatus() {
  clearInterval(verifyPollTimer);
  verifyPollTimer = setInterval(async () => {
    try {
      const status = await api('/api/v1/mobile/verify-phone/status', { auth: true });
      if (status.verified) {
        clearInterval(verifyPollTimer);
        document.getElementById('verify-phone-waiting').textContent = '✅ Verified!';
        const resultEl = document.getElementById('verify-phone-result');
        resultEl.textContent = 'ফোন নম্বর successfully verify হয়েছে।';
        resultEl.className = 'result-msg ok';
      }
    } catch (e) {
      // transient — next poll tries again
    }
  }, 3000);
}

// ============================================================
// P2P Trading — Binance-style: browse ads, dedicated order-form screen
// (no browser prompt()s), a step-timeline order detail view, and a proper
// cancel-reason picker.
// ============================================================

let p2pCurrentAd = null;   // ad being ordered against, while on the order-form screen
let p2pCurrentOrderId = null; // order being viewed/acted on, while on the detail screen
let p2pDetailPollTimer = null;

function openP2P() {
  openSheet('p2p');
  showP2PView('main');
  switchP2PTab('browse');
}

function showP2PView(view) {
  document.getElementById('p2p-main-view').style.display = view === 'main' ? 'block' : 'none';
  document.getElementById('p2p-order-form-view').style.display = view === 'order-form' ? 'block' : 'none';
  document.getElementById('p2p-order-detail-view').style.display = view === 'order-detail' ? 'block' : 'none';
  document.getElementById('p2p-cancel-view').style.display = view === 'cancel' ? 'block' : 'none';
  if (view !== 'order-detail') clearInterval(p2pDetailPollTimer);
}

function backToP2PMain() {
  showP2PView('main');
  loadMyOrders();
}
function backToOrderDetail() {
  showP2PView('order-detail');
}

function switchP2PTab(tab) {
  ['browse', 'myads', 'orders'].forEach((t) => {
    document.getElementById('p2p-tab-' + t).classList.toggle('active', t === tab);
    document.getElementById('p2p-' + t).style.display = t === tab ? 'block' : 'none';
  });
  if (tab === 'myads') loadMyAds();
  if (tab === 'orders') loadMyOrders();
}

function p2pResult(msg, ok) {
  const el = document.getElementById('p2p-result');
  el.textContent = msg;
  el.className = 'result-msg ' + (ok ? 'ok' : 'err');
}

// --- Browse & create order ---

async function loadAds(side) {
  document.getElementById('p2p-side-buy').classList.toggle('active-buy', side === 'sell');
  document.getElementById('p2p-side-sell').classList.toggle('active-sell', side === 'buy');
  const listEl = document.getElementById('p2p-ads-list');
  listEl.innerHTML = '<div class="empty-state">লোড হচ্ছে...</div>';
  try {
    const ads = await api('/api/v1/mobile/p2p/ads?side=' + side);
    if (!ads.length) {
      listEl.innerHTML = '<div class="empty-state">এই মুহূর্তে কোনো ad নেই</div>';
      return;
    }
    listEl.innerHTML = ads.map((ad) => `
      <div class="p2p-ad-card">
        <div class="price-row">
          <div class="price">৳${ad.price} <small>/ BDT</small></div>
          <button class="green" style="margin-top:0;padding:8px 16px;width:auto;" onclick='openOrderForm(${JSON.stringify(ad)})'>Order দাও</button>
        </div>
        <div class="limit-row">Limit: ${ad.min_order} – ${ad.max_order} BDT · অবশিষ্ট ${ad.remaining_amount} BDT</div>
        <div class="methods">${ad.payment_methods.split(',').map((m) => `<span class="method-chip">${m.trim()}</span>`).join('')}</div>
      </div>
    `).join('');
  } catch (e) {
    listEl.innerHTML = '<div class="empty-state">লোড করা যায়নি</div>';
  }
}

function openOrderForm(ad) {
  p2pCurrentAd = ad;
  document.getElementById('p2p-order-form-ad-info').innerHTML = `
    <div class="price-row"><div class="price">৳${ad.price} <small>/ BDT</small></div></div>
    <div class="limit-row">Limit: ${ad.min_order} – ${ad.max_order} BDT</div>
  `;
  const methodSelect = document.getElementById('order-method');
  methodSelect.innerHTML = ad.payment_methods.split(',').map((m) => `<option value="${m.trim()}">${m.trim()}</option>`).join('');
  document.getElementById('order-amount').value = '';
  showP2PView('order-form');
}

async function submitOrder() {
  const amount = document.getElementById('order-amount').value;
  const method = document.getElementById('order-method').value;
  if (!amount || Number(amount) <= 0) {
    alert('সঠিক amount দাও');
    return;
  }
  try {
    const data = await api('/api/v1/mobile/p2p/orders', {
      method: 'POST', auth: true,
      body: { ad_id: p2pCurrentAd.id, amount, payment_method: method },
    });
    await openOrderDetail(data.order_id);
  } catch (e) {
    alert(e.message);
  }
}

// --- Order detail (step timeline) ---

async function openOrderDetail(orderId) {
  p2pCurrentOrderId = orderId;
  showP2PView('order-detail');
  await renderOrderDetail();
  clearInterval(p2pDetailPollTimer);
  p2pDetailPollTimer = setInterval(renderOrderDetail, 5000);
}

async function renderOrderDetail() {
  const body = document.getElementById('p2p-order-detail-body');
  let order;
  try {
    order = await api('/api/v1/mobile/p2p/orders/' + p2pCurrentOrderId, { auth: true });
  } catch (e) {
    body.innerHTML = '<div class="empty-state">Order লোড করা যায়নি</div>';
    return;
  }

  const myId = Number(session.userId);
  const iAmProvider = order.token_provider_id === myId;
  const remainingMs = new Date(order.payment_deadline).getTime() - Date.now();
  const remainingMin = Math.max(0, Math.floor(remainingMs / 60000));
  const remainingSec = Math.max(0, Math.floor((remainingMs % 60000) / 1000));

  let statusLabel = { pending_payment: 'Payment-এর অপেক্ষায়', paid: 'Provider confirm করার অপেক্ষায়', completed: 'সম্পন্ন ✅', cancelled: 'বাতিল', disputed: 'Dispute-এ আছে' }[order.status] || order.status;

  let actionsHtml = '';
  if (order.status === 'pending_payment') {
    if (!iAmProvider) {
      actionsHtml += `<button class="green" onclick="markPaid(${order.id})">টাকা পাঠিয়েছি — Mark as Paid</button>`;
    }
    actionsHtml += `<button class="ghost" onclick="showP2PView('cancel')">Cancel Order</button>`;
  }
  if ((order.status === 'paid' || order.status === 'pending_payment') && iAmProvider) {
    actionsHtml += `<button class="green" onclick="confirmOrder(${order.id})">টাকা পেয়েছি, Release করো</button>`;
  }
  if (order.status === 'paid' || order.status === 'pending_payment') {
    actionsHtml += `<button class="ghost" onclick="promptDispute(${order.id})">Dispute খোলো</button>`;
  }

  body.innerHTML = `
    <h2>Order #${order.id}</h2>
    ${order.status === 'pending_payment' ? `<div class="countdown">Pay within ${remainingMin}:${String(remainingSec).padStart(2, '0')}</div>` : `<div class="hint">${statusLabel}</div>`}

    <div class="p2p-step" data-step="1">
      <div class="p2p-step-body">
        <b>${iAmProvider ? 'তুমি BDT দিচ্ছো, টাকা পাবে' : 'তুমি টাকা দিচ্ছো, BDT পাবে'}</b>
        <div class="p2p-field"><span class="k">Amount</span><span class="v">${order.amount} BDT</span></div>
        <div class="p2p-field"><span class="k">দাম</span><span class="v">৳${order.price} / BDT</span></div>
        <div class="p2p-field"><span class="k">তুমি ${iAmProvider ? 'পাবে' : 'দিবে'}</span><span class="v">৳${order.fiat_amount}</span></div>
        <div class="p2p-field"><span class="k">Payment method</span><span class="v">${order.payment_method}</span></div>
      </div>
    </div>
    <div class="p2p-step" data-step="2">
      <div class="p2p-step-body">
        <b>Status</b>
        <div class="p2p-field"><span class="k">এই মুহূর্তে</span><span class="v">${statusLabel}</span></div>
      </div>
    </div>

    <div style="display:flex;flex-direction:column;gap:8px;margin-top:10px;">${actionsHtml}</div>
  `;
}

function p2pDetailResult(msg, ok) {
  const el = document.getElementById('p2p-detail-result');
  el.textContent = msg;
  el.className = 'result-msg ' + (ok ? 'ok' : 'err');
}

async function markPaid(orderId) {
  try {
    await api(`/api/v1/mobile/p2p/orders/${orderId}/mark-paid`, { method: 'POST', auth: true });
    p2pDetailResult('Payment মার্ক করা হয়েছে — provider confirm করার অপেক্ষায়', true);
    renderOrderDetail();
  } catch (e) { p2pDetailResult(e.message, false); }
}

async function confirmOrder(orderId) {
  if (!confirm('তুমি কি নিশ্চিত real টাকা তোমার bKash/bank-এ এসেছে? এটা করলে BDT release হয়ে যাবে, ফেরত আনা যাবে না।')) return;
  try {
    await api(`/api/v1/mobile/p2p/orders/${orderId}/confirm`, { method: 'POST', auth: true });
    p2pDetailResult('Order সম্পন্ন! BDT release হয়েছে।', true);
    renderOrderDetail();
    loadBalance();
  } catch (e) { p2pDetailResult(e.message, false); }
}

async function confirmCancelWithReason() {
  const selected = document.querySelector('input[name="cancel-reason"]:checked');
  if (!selected) {
    alert('একটা কারণ বেছে নাও');
    return;
  }
  try {
    await api(`/api/v1/mobile/p2p/orders/${p2pCurrentOrderId}/cancel`, { method: 'POST', auth: true });
    showP2PView('order-detail');
    p2pDetailResult('Order cancel হয়েছে (কারণ: ' + selected.value + ')', true);
    renderOrderDetail();
  } catch (e) {
    showP2PView('order-detail');
    p2pDetailResult(e.message, false);
  }
}

async function promptDispute(orderId) {
  const reason = prompt('Dispute-এর কারণ লিখো:');
  if (!reason) return;
  try {
    await api(`/api/v1/mobile/p2p/orders/${orderId}/dispute`, { method: 'POST', auth: true, body: { reason } });
    p2pDetailResult('Dispute খোলা হয়েছে, admin review করবে।', true);
    renderOrderDetail();
  } catch (e) { p2pDetailResult(e.message, false); }
}

// --- My ads ---

function showCreateAdForm() {
  document.getElementById('p2p-create-ad-form').style.display = 'block';
}

async function createAd() {
  const body = {
    side: document.getElementById('ad-side').value,
    price: document.getElementById('ad-price').value,
    min_order: document.getElementById('ad-min').value,
    max_order: document.getElementById('ad-max').value,
    total_amount: document.getElementById('ad-total').value,
    payment_methods: document.getElementById('ad-methods').value,
  };
  try {
    await api('/api/v1/mobile/p2p/ads', { method: 'POST', auth: true, body });
    p2pResult('Ad পোস্ট হয়েছে!', true);
    document.getElementById('p2p-create-ad-form').style.display = 'none';
    loadMyAds();
  } catch (e) {
    p2pResult(e.message, false);
  }
}

async function loadMyAds() {
  const listEl = document.getElementById('p2p-my-ads-list');
  listEl.innerHTML = '<div class="empty-state">লোড হচ্ছে...</div>';
  try {
    const ads = await api('/api/v1/mobile/p2p/ads/mine', { auth: true });
    if (!ads.length) {
      listEl.innerHTML = '<div class="empty-state">তোমার কোনো ad নেই</div>';
      return;
    }
    listEl.innerHTML = ads.map((ad) => `
      <div class="p2p-ad-card">
        <div class="price-row">
          <div class="price">${ad.side === 'sell' ? 'Sell' : 'Buy'} @ ৳${ad.price}</div>
          <span class="method-chip">${ad.status}</span>
        </div>
        <div class="limit-row">অবশিষ্ট: ${ad.remaining_amount} / ${ad.total_amount} BDT</div>
        ${ad.status === 'active' ? `<button class="ghost" style="margin-top:10px;" onclick="cancelAd(${ad.id})">Ad Cancel করো</button>` : ''}
      </div>
    `).join('');
  } catch (e) {
    listEl.innerHTML = '<div class="empty-state">লোড করা যায়নি</div>';
  }
}

async function cancelAd(adId) {
  try {
    await api(`/api/v1/mobile/p2p/ads/${adId}/cancel`, { method: 'POST', auth: true });
    p2pResult('Ad cancel হয়েছে', true);
    loadMyAds();
  } catch (e) {
    p2pResult(e.message, false);
  }
}

// --- My orders ---

async function loadMyOrders() {
  const listEl = document.getElementById('p2p-orders-list');
  listEl.innerHTML = '<div class="empty-state">লোড হচ্ছে...</div>';
  try {
    const orders = await api('/api/v1/mobile/p2p/orders', { auth: true });
    if (!orders.length) {
      listEl.innerHTML = '<div class="empty-state">তোমার কোনো order নেই</div>';
      return;
    }
    const myId = Number(session.userId);
    listEl.innerHTML = orders.map((o) => {
      const iAmProvider = o.token_provider_id === myId;
      const role = iAmProvider ? 'তুমি BDT দিচ্ছো' : 'তুমি টাকা দিচ্ছো';
      const statusLabel = { pending_payment: 'Payment-এর অপেক্ষায়', paid: 'Confirm-এর অপেক্ষায়', completed: 'সম্পন্ন', cancelled: 'বাতিল', disputed: 'Dispute-এ' }[o.status] || o.status;
      return `
        <div class="p2p-ad-card" style="cursor:pointer" onclick="openOrderDetail(${o.id})">
          <div class="price-row">
            <div class="price">#${o.id} — ${o.amount} BDT</div>
            <span class="method-chip">${statusLabel}</span>
          </div>
          <div class="limit-row">${role} · ৳${o.fiat_amount} via ${o.payment_method}</div>
        </div>
      `;
    }).join('');
  } catch (e) {
    listEl.innerHTML = '<div class="empty-state">লোড করা যায়নি</div>';
  }
}
