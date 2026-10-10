#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""让 opencode2api 的池刷新器把 GoProxy 导出的代理文件一起并入 proxies.txt。

背景：opencode2api 的 proxyscraper_refresh.py 自己维护 proxies_public.txt，
并把 master + public 合并写回 proxies.txt。因此任何直接写 proxies_public.txt
的外部注入都会被下一轮刷新覆盖（实测：注入 26 条，刷新后区块消失）。

本补丁以**最小侵入**方式解决：新增一个独立文件 proxies_goproxy.txt（由
deploy/export-to-opencode2.sh 维护），并让刷新器在合并时一并读入。
幂等：重复执行不会重复插入。

用法：
  python3 patch-opencode2-refresh.py [--path ~/opencode2api/proxyscraper_refresh.py] [--check]
"""
import argparse
import pathlib
import py_compile
import shutil
import sys
import time

MARK = "# >>> goproxy-pool:merge"
CONST_ANCHOR = 'STATE = f"{BASE}/proxyscraper_state.json"'
CONST_LINE = (
    f'{MARK}\n'
    'GOPROXY = f"{BASE}/proxies_goproxy.txt"   # 由 GoProxy 导出脚本维护（可选）\n'
)
MERGE_OLD = "        combined = list(dict.fromkeys(master + pub_urls))"
# 第一版补丁写法（未过滤注释行，会被 valid_proxy() 判为非法项导致刷新中止）→ 需要就地升级
MERGE_V1 = (
    "        # GoProxy 导出池（可选）：文件不存在时 load_lines 返回空列表，不影响原有行为\n"
    "        combined = list(dict.fromkeys(master + pub_urls + load_lines(GOPROXY)))"
)
MERGE_NEW = (
    "        # GoProxy 导出池（可选）：文件不存在时 load_lines 返回空列表；\n"
    "        # 必须过滤 '#' 注释行与空行，否则 valid_proxy() 会判定非法并中止整轮刷新\n"
    "        goproxy_urls = [x.strip() for x in load_lines(GOPROXY)\n"
    "                        if x.strip() and not x.strip().startswith('#')]\n"
    "        combined = list(dict.fromkeys(master + pub_urls + goproxy_urls))"
)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--path", default=str(pathlib.Path.home() / "opencode2api" / "proxyscraper_refresh.py"))
    ap.add_argument("--check", action="store_true", help="只检查是否已打补丁")
    args = ap.parse_args()

    p = pathlib.Path(args.path).expanduser()
    if not p.exists():
        print(f"[!] 找不到 {p}")
        return 2
    src = p.read_text(encoding="utf-8")

    if MERGE_NEW in src:
        print(f"[=] 已是最新补丁：{p}")
        return 0
    if args.check:
        print(f"[!] 未打补丁或需要升级：{p}")
        return 1

    backup = p.with_suffix(f".py.bak-goproxy-{time.strftime('%Y%m%d-%H%M%S')}")
    shutil.copy2(p, backup)

    patched = src
    if MERGE_V1 in patched:
        # 就地升级第一版补丁（只替换合并表达式，常量行保留）
        patched = patched.replace(MERGE_V1, MERGE_NEW, 1)
        action = "升级"
    elif CONST_ANCHOR in patched and MERGE_OLD in patched:
        patched = patched.replace(CONST_ANCHOR, CONST_ANCHOR + "\n" + CONST_LINE.rstrip("\n"), 1)
        patched = patched.replace(MERGE_OLD, MERGE_NEW, 1)
        action = "首次应用"
    else:
        print("[!] 锚点未命中，脚本可能已被改动，放弃（请人工确认）")
        return 3

    p.write_text(patched, encoding="utf-8")

    try:
        py_compile.compile(str(p), doraise=True)
    except py_compile.PyCompileError as e:
        shutil.copy2(backup, p)
        print(f"[!] 语法检查失败，已回滚：{e}")
        return 4

    print(f"[+] 已{action}补丁：{p}\n    备份：{backup}")
    print("    合并逻辑：master + public + proxies_goproxy.txt(去注释) → proxies.txt")
    return 0


if __name__ == "__main__":
    sys.exit(main())
