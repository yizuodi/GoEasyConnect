async function monitorEasyConnectUpdate(request, target, button, previousUpdateAt) {
  const phases = {
    starting: '准备更新', 'release-query': '查询版本', download: '下载更新', verify: '校验安装包',
    'resolve-database': '读取配置', backup: '备份数据', install: '安装更新',
    'health-check': '检查健康状态', restart: '重启服务', complete: '更新完成'
  };
  const deadline = Date.now() + 15 * 60 * 1000;
  while (Date.now() < deadline) {
    await new Promise(resolve => setTimeout(resolve, 3000));
    try {
      const response = await request('/api/update/progress', {signal: AbortSignal.timeout(8000)});
      if (response.status === 401) throw new Error('登录已失效，请重新登录后检查版本。');
      if (!response.ok) continue;
      const progress = await response.json();
      // Ignore a previous attempt's result until this update has written its status.
      if (progress.updatedAt && progress.updatedAt <= previousUpdateAt) continue;
      if (button) button.textContent = phases[progress.phase] || '更新中…';
      if (progress.state === 'failed') {
        alert(`更新失败（${phases[progress.phase] || progress.phase || '启动阶段'}）。旧版本可能仍在运行，请检查更新器日志。\n${progress.error || ''}`);
        return;
      }
      if (progress.state === 'succeeded' && progress.currentVersion === target) {
        alert(`已成功更新至 ${target}，页面即将刷新。`);
        location.reload();
        return;
      }
    } catch (error) {
      if (error.message?.startsWith('登录已失效')) { alert(error.message); return; }
      if (button) button.textContent = '等待服务恢复…';
    }
  }
  alert('更新尚未确认完成。请重新检查当前版本，或查看 journalctl -u goeasyconnect-updater.service -n 80 --no-pager。');
}
