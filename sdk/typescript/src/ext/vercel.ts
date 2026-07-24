// Vercel AI SDK adapter — chat message persistence backed by memsidecar.
//
// The AI SDK persists a chat as its full `UIMessage[]`, loaded by id at the
// start of a request and re-saved after each turn. This adapter implements that
// convention on the kv block: one key per chat holding the JSON message array.
// Import it from the "/ai" subpath so the base client never pulls in `ai`:
//
//   import { MemSidecar } from "@memsidecar/client";
//   import { createChatStore } from "@memsidecar/client/ai";
//
//   const store = createChatStore(new MemSidecar(addr, { token }));
//   const chatId = await store.createChat();
//   const messages = await store.loadChat(chatId);
//
// Wire it into a streamed response so each turn is saved (AI SDK v5+):
//
//   return result.toUIMessageStreamResponse({
//     originalMessages: messages,
//     onFinish: ({ messages }) => store.saveChat({ chatId, messages }),
//   });
import { randomUUID } from "node:crypto";

import type { UIMessage } from "ai";

/**
 * The subset of a `MemSidecar` client this store uses — its `kv` block. A full
 * `MemSidecar` instance satisfies it structurally, and it keeps the store easy
 * to unit-test with a fake.
 */
export interface ChatStoreClient {
  kv: {
    get(namespace: string, key: string): Promise<{ found: boolean; value: Uint8Array }>;
    put(
      namespace: string,
      key: string,
      value: Uint8Array,
      opts?: { contentType?: string; ttlSeconds?: number },
    ): Promise<unknown>;
    delete(namespace: string, key: string): Promise<{ existed: boolean }>;
    scan(namespace: string, opts?: { keyPrefix?: string }): AsyncIterable<{ key: string }>;
  };
}

export interface MemSidecarChatStoreOptions {
  /** kv namespace holding the chats. Default "chats". */
  namespace?: string;
  /** New-chat id generator. Default `crypto.randomUUID()`. */
  generateId?: () => string;
}

/**
 * A chat store shaped for the AI SDK's persistence points: `loadChat`/`saveChat`
 * match its convention, plus `createChat`/`deleteChat`/`listChats`.
 */
export interface MemSidecarChatStore {
  /** Create an empty chat and return its id. */
  createChat(): Promise<string>;
  /** Load a chat's messages; returns [] for an unknown/empty chat. */
  loadChat(id: string): Promise<UIMessage[]>;
  /** Replace a chat's messages (call after each turn). */
  saveChat(args: { chatId: string; messages: UIMessage[] }): Promise<void>;
  /** Delete a chat; returns whether it existed. */
  deleteChat(id: string): Promise<boolean>;
  /** List every chat id in the namespace. */
  listChats(): Promise<string[]>;
}

const enc = new TextEncoder();
const dec = new TextDecoder();

/** Build a memsidecar-backed AI SDK chat store over the kv block. */
export function createChatStore(
  client: ChatStoreClient,
  opts: MemSidecarChatStoreOptions = {},
): MemSidecarChatStore {
  const namespace = opts.namespace ?? "chats";
  const generateId = opts.generateId ?? defaultId;

  async function saveChat(args: { chatId: string; messages: UIMessage[] }): Promise<void> {
    const body = enc.encode(JSON.stringify(args.messages));
    await client.kv.put(namespace, args.chatId, body, { contentType: "application/json" });
  }

  async function loadChat(id: string): Promise<UIMessage[]> {
    const rec = await client.kv.get(namespace, id);
    if (!rec.found || rec.value.length === 0) {
      return [];
    }
    return JSON.parse(dec.decode(rec.value)) as UIMessage[];
  }

  async function createChat(): Promise<string> {
    const id = generateId();
    await saveChat({ chatId: id, messages: [] });
    return id;
  }

  async function deleteChat(id: string): Promise<boolean> {
    return (await client.kv.delete(namespace, id)).existed;
  }

  async function listChats(): Promise<string[]> {
    const ids: string[] = [];
    for await (const item of client.kv.scan(namespace, {})) {
      ids.push(item.key);
    }
    return ids;
  }

  return { createChat, loadChat, saveChat, deleteChat, listChats };
}

function defaultId(): string {
  return randomUUID();
}
