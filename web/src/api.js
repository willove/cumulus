export const jsonPost = body => ({
  method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
});

export async function requestJSON(url, options) {
  const response = await fetch(url, options);
  const text = await response.text();
  let data;
  try { data = text ? JSON.parse(text) : null; } catch {
    throw new Error(response.ok ? "服务返回了无效的 JSON" : "HTTP " + response.status);
  }
  if (!response.ok) {
    throw new Error((data?.error || "HTTP " + response.status) + (data?.hint ? "（" + data.hint + "）" : ""));
  }
  return data;
}

export const api = {
  ingest: body => requestJSON("/v1/ingest/jobs", jsonPost(body)),
  scan: body => requestJSON("/v1/scan", jsonPost(body)),
  probe: paths => requestJSON("/v1/adapt/probe", jsonPost({ paths })),
  adapt: body => requestJSON("/v1/adapt/ingest", jsonPost(body)),
  upload(namespace, files) {
    const body = new FormData();
    for (const file of files) body.append("files", file.raw, file.name);
    return requestJSON("/v1/ingest/upload?ns=" + encodeURIComponent(namespace), { method: "POST", body });
  },
  searchStream: (body, signal) => fetch("/v1/search/stream", { ...jsonPost(body), signal }),
};

export function evaluationURL(path, namespace) {
  if (!namespace) throw new Error("请先创建或选择知识库");
  return "/v1/eval/" + path + (path.includes("?") ? "&" : "?") + "ns=" + encodeURIComponent(namespace);
}

export const evaluationAPI = {
  get: (path, namespace, signal) => requestJSON(evaluationURL(path, namespace), { signal }),
  post: (path, namespace, body, signal) => requestJSON(evaluationURL(path, namespace), { ...jsonPost(body), signal }),
  async download(id, format, namespace, signal) {
    const response = await fetch(evaluationURL("runs/" + encodeURIComponent(id) + "/export?format=" + encodeURIComponent(format), namespace), { signal });
    if (!response.ok) {
      let data;
      try { data = await response.json(); } catch {}
      throw new Error(data?.error || "导出失败：HTTP " + response.status);
    }
    return response.blob();
  },
};
