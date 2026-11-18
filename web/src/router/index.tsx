import { lazy } from "react";
import { createHashRouter, Navigate } from "react-router-dom";
import { AppShell } from "@/layouts/AppShell";

const Dashboard = lazy(() => import("@/views/overview/Dashboard"));
const Goals = lazy(() => import("@/views/goals/Goals"));
const GoalDetail = lazy(() => import("@/views/goals/GoalDetail"));
const Items = lazy(() => import("@/views/work/Items"));
const ItemDetail = lazy(() => import("@/views/work/ItemDetail"));
const Models = lazy(() => import("@/views/models/Models"));
const Usage = lazy(() => import("@/views/insight/Usage"));
const Scores = lazy(() => import("@/views/insight/Scores"));
const Login = lazy(() => import("@/views/system/Login"));

// hash 路由：静态文件由 Go embed 提供，不需要服务端 rewrite。
export const router = createHashRouter([
  { path: "/login", element: <Login /> },
  {
    path: "/",
    element: <AppShell />,
    children: [
      { index: true, element: <Dashboard /> },
      { path: "goals", element: <Goals /> },
      { path: "goals/:id", element: <GoalDetail /> },
      { path: "items", element: <Items /> },
      { path: "items/:id", element: <ItemDetail /> },
      { path: "models", element: <Models /> },
      { path: "usage", element: <Usage /> },
      { path: "scores", element: <Scores /> },
    ],
  },
  { path: "*", element: <Navigate to="/" replace /> },
]);
