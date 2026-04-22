from fastapi import FastAPI
import os
import uvicorn
import requests

app = FastAPI()

QWEN_BASE_URL = os.environ.get("QWEN_BASE_URL", "http://localhost:8085")


def _headers():
    return {"Content-Type": "application/json", "Authorization": "Bearer test-key"}


@app.get("/health")
async def health():
    return "ok!"


@app.get("/chat")
async def chat():
    payload = {
        "model": "qwen-plus",
        "messages": [
            {"role": "system", "content": "You are a helpful assistant."},
            {"role": "user", "content": "你是谁？"},
        ],
    }
    resp = requests.post(
        f"{QWEN_BASE_URL}/compatible-mode/v1/chat/completions",
        json=payload,
        headers=_headers(),
    )
    resp.raise_for_status()
    return resp.json()


@app.get("/generation")
async def generation():
    payload = {
        "model": "qwen-turbo",
        "prompt": "Explain eBPF in one sentence.",
    }
    resp = requests.post(
        f"{QWEN_BASE_URL}/api/v1/services/aigc/text-generation/generation",
        json=payload,
        headers=_headers(),
    )
    resp.raise_for_status()
    return resp.json()


@app.get("/error")
async def error_call():
    payload = {
        "model": "qwen-plus",
        "messages": [
            {"role": "user", "content": "trigger error"},
        ],
    }
    resp = requests.post(
        f"{QWEN_BASE_URL}/compatible-mode/v1/chat/completions?error=true",
        json=payload,
        headers=_headers(),
    )
    return resp.json()


if __name__ == "__main__":
    print(f"Qwen test server running: port=8080 pid={os.getpid()}")
    uvicorn.run(app, host="0.0.0.0", port=8080)
