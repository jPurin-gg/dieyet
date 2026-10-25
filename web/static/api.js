// Thin JSON client for /api. Throws ApiError with the server's message.

export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function call(method, path, body) {
  const init = { method, headers: {} };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const res = await fetch(path, init);
  if (res.status === 204) return null;
  let data = null;
  try { data = await res.json(); } catch (e) { /* non-JSON body */ }
  if (!res.ok) {
    throw new ApiError(res.status, (data && data.error) || `エラー (${res.status})`);
  }
  return data;
}

export const api = {
  me: () => call('GET', '/api/me'),
  login: (nickname, passphrase) => call('POST', '/api/login', { nickname, passphrase }),
  logout: () => call('POST', '/api/logout'),
  product: (barcode) => call('GET', `/api/products/${encodeURIComponent(barcode)}`),
  putProduct: (barcode, product) => call('PUT', `/api/products/${encodeURIComponent(barcode)}`, product),
  logs: (date) => call('GET', `/api/logs?date=${encodeURIComponent(date)}`),
  addLog: (entry) => call('POST', '/api/logs', entry),
  deleteLog: (id) => call('DELETE', `/api/logs/${id}`),
};
