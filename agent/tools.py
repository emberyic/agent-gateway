"""本地工具：时间与天气。"""

from __future__ import annotations

import os
from datetime import datetime

import httpx
from langchain.tools import tool
from langchain_core.tools import BaseTool

# 和风天气：城市名 → Location ID → 实时天气
def build_time_tool() -> BaseTool:
    """当前时间工具。"""

    @tool
    def get_current_time() -> str:
        """获取当前的系统日期和时间。当用户问现在几点、今天几号时, 调用此工具。"""
        return datetime.now().strftime("%Y-%m-%d %H:%M:%S")

    return get_current_time


def build_weather_tool(api_key: str, host: str) -> BaseTool:
    """天气工具。api_key 和 host 在装配期注入，工具内不再自己读环境变量。"""
    geo_url = f"https://{host}/geo/v2/city/lookup"
    weather_url = f"https://{host}/v7/weather/now"

    @tool
    async def get_weather(city: str) -> str:
        """获取指定城市的天气。"""
        key = api_key
        if not key:
            return "未配置 QWEATHER_API_KEY, 无法查询天气。"

        # trust_env=False：忽略系统代理设置，避免本地代理把请求劫持成失败
        async with httpx.AsyncClient(trust_env=False, timeout=10.0) as client:
            try:
                geo_resp = await client.get(geo_url, params={"location": city, "key": key})
                geo_data = geo_resp.json()
                if geo_data.get("code") == "200" and geo_data.get("location"):
                    location_id = geo_data["location"][0]["id"]
                else:
                    return f"GeoAPI 查询失败，错误码: {geo_data.get('code')}, 响应: {geo_resp.text[:200]}"

                weather_resp = await client.get(weather_url, params={"location": location_id, "key": key})
                weather_data = weather_resp.json()
                if weather_data.get("code") != "200":
                    return f"查询 {city} 天气失败, API错误码: {weather_data.get('code')}"

                now = weather_data["now"]
                return (
                    f"{city} 当前天气是 {now['text']}, 温度{now['temp']}℃, "
                    f"体感温度{now['feelsLike']}℃, 风向{now['windDir']}, 风力{now['windScale']}级。"
                )
            except Exception as exc:  # noqa: BLE001 — 工具绝不抛异常（见模块文档）
                return f"查询天气时发生错误: {type(exc).__name__}: {exc}"

    return get_weather
