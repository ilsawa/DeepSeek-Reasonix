!macro rxScript
  FileOpen $0 "$PLUGINSDIR\rxclose.ps1" w
  FileWrite $0 "param([string]$$Dir, [string]$$Mode)$\r$\n"
  FileWrite $0 "$$names = @('${APP_EXECUTABLE_FILENAME}', 'reasonix-studio-host.exe', 'cua-driver.exe')$\r$\n"
  FileWrite $0 "$$hit = @(Get-CimInstance -ClassName Win32_Process | ? { ($$_.Path -and $$_.Path.StartsWith($$Dir, 'CurrentCultureIgnoreCase')) -or (-not $$_.Path -and $$names -contains $$_.Name) })$\r$\n"
  FileWrite $0 "if ($$Mode -eq 'find') { if ($$hit.Count -gt 0) { exit 0 } else { exit 1 } }$\r$\n"
  FileWrite $0 "$$hit | % { Stop-Process -Id $$_.ProcessId -Force -ErrorAction SilentlyContinue }$\r$\n"
  FileClose $0
!macroend

!macro rxFind
  nsExec::Exec `"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$PLUGINSDIR\rxclose.ps1" "$INSTDIR" find`
  Pop $R0
!macroend

!macro rxKill
  nsExec::Exec `"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$PLUGINSDIR\rxclose.ps1" "$INSTDIR" kill`
  Pop $0
!macroend

!macro rxKillElevated
  nsExec::Exec `"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -NonInteractive -C "Start-Process -FilePath powershell -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File','$PLUGINSDIR\rxclose.ps1','$INSTDIR','kill' -Verb RunAs -Wait -WindowStyle Hidden"`
  Pop $0
!macroend

!macro customCheckAppRunning
  InitPluginsDir
  !insertmacro rxScript
  !insertmacro rxFind
  ${if} $R0 == 0
    ${ifNot} ${isUpdated}
    ${andIfNot} ${Silent}
      DetailPrint "$(appClosing)"
    ${endIf}
    StrCpy $R1 0
    rxCloseLoop:
      IntOp $R1 $R1 + 1
      !insertmacro rxKill
      Sleep 1000
      !insertmacro rxFind
      ${if} $R0 != 0
        Goto rxClosed
      ${endIf}
      ${if} $R1 == 4
      ${andIfNot} ${Silent}
        DetailPrint "Asking Windows for permission to close the elevated copy of ${PRODUCT_NAME}..."
        !insertmacro rxKillElevated
        Sleep 1000
        !insertmacro rxFind
        ${if} $R0 != 0
          Goto rxClosed
        ${endIf}
      ${endIf}
      ${if} $R1 < 6
        Goto rxCloseLoop
      ${endIf}
    StrCmp $LANGUAGE 2052 rxZh
      MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "${PRODUCT_NAME} is still running and the installer could not close it.$\r$\n$\r$\nRight-click its icon in the system tray (click the ^ if it is hidden) and choose Quit, or press Ctrl+Shift+Esc, end every process named $\"${PRODUCT_NAME}$\" (run Task Manager as administrator if the app was started as administrator), then click Retry." /SD IDCANCEL IDRETRY rxRetry
      Quit
    rxZh:
      MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "${PRODUCT_NAME} 仍在运行，安装程序无法自动关闭它。$\r$\n$\r$\n请在任务栏右下角的托盘图标（看不到就点 ^）上右键，选择“退出”；或按 Ctrl+Shift+Esc 打开任务管理器，结束所有名为“${PRODUCT_NAME}”的进程（如果它是以管理员身份启动的，请用管理员身份打开任务管理器），然后点“重试”。" /SD IDCANCEL IDRETRY rxRetry
      Quit
    rxRetry:
      StrCpy $R1 0
      Goto rxCloseLoop
    rxClosed:
  ${endIf}
!macroend
