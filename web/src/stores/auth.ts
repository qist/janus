import { create } from "zustand";
import { persist } from "zustand/middleware";

type AuthState = {
  key: string;
  setKey: (key: string) => void;
  clear: () => void;
};

// 控制台用 API key 认证。key 只保存在本机浏览器，服务端没有会话。
export const useAuth = create<AuthState>()(
  persist(
    (set) => ({
      key: "",
      setKey: (key: string) => set({ key }),
      clear: () => set({ key: "" }),
    }),
    { name: "janus-auth" }
  )
);
