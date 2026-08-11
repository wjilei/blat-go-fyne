; scripts/installer.nsi
; BLAT 测试程序 NSIS 安装脚本
; 用法：makensis /DVERSION=x.y.z scripts\installer.nsi
; 产物：dist\BLAT-Setup-x.y.z.exe

!include "MUI2.nsh"
!include "FileFunc.nsh"
!include "x64.nsh"

; -------- 元信息 --------
!define PRODUCT_NAME      "BLAT 测试程序"
!define PRODUCT_PUBLISHER "BLAT"
!define PRODUCT_EXE       "blat.exe"
!define UNINST_KEY        "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}"

!ifndef VERSION
  !define VERSION "0.0.0-dev"
!endif

Name "${PRODUCT_NAME} ${VERSION}"
OutFile "..\dist\BLAT-Setup-${VERSION}.exe"
InstallDir "$PROGRAMFILES64\BLAT"
InstallDirRegKey HKLM "Software\BLAT" "InstallDir"
RequestExecutionLevel highest
ShowInstDetails show
ShowUninstDetails show

BrandingText "${PRODUCT_NAME}"

; -------- Modern UI --------
!define MUI_ABORTWARNING
!define MUI_ICON "..\app.ico"
!define MUI_UNICON "..\app.ico"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_WELCOME
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_UNPAGE_FINISH

!insertmacro MUI_LANGUAGE "SimpChinese"

; -------- 安装 --------
Section "主程序（必装）" SecMain
  SectionIn RO
  SetOutPath "$INSTDIR"
  File "..\bin\${PRODUCT_EXE}"

  SetOutPath "$INSTDIR\confs"
  File /r "..\confs\*.*"
  ; env.yml 已迁到用户家目录 ~/.blat/env.yml（uploader.uuid.txt 同目录）——
  ; 装到 $INSTDIR\confs\ 反而误导用户去找（且安装目录无写权限），删掉。
  Delete "$INSTDIR\confs\env.yml"

  SetOutPath "$INSTDIR"

  ; 预先创建用户配置目录，避免首次启动保存 MBUS 串口时报"父目录不存在"。
  ; $PROFILE = %USERPROFILE% (e.g. C:\Users\xxx)。权限是当前用户的，写入无碍。
  CreateDirectory "$PROFILE\.blat"

  WriteRegStr HKLM "Software\BLAT" "InstallDir" "$INSTDIR"

  WriteUninstaller "$INSTDIR\Uninstall.exe"
  WriteRegStr   HKLM "${UNINST_KEY}" "DisplayName"     "${PRODUCT_NAME}"
  WriteRegStr   HKLM "${UNINST_KEY}" "DisplayVersion"  "${VERSION}"
  WriteRegStr   HKLM "${UNINST_KEY}" "Publisher"       "${PRODUCT_PUBLISHER}"
  WriteRegStr   HKLM "${UNINST_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr   HKLM "${UNINST_KEY}" "UninstallString" "$INSTDIR\Uninstall.exe"
  WriteRegStr   HKLM "${UNINST_KEY}" "DisplayIcon"     "$INSTDIR\${PRODUCT_EXE}"
  WriteRegDWORD HKLM "${UNINST_KEY}" "NoModify" 1
  WriteRegDWORD HKLM "${UNINST_KEY}" "NoRepair" 1

  ${GetSize} "$INSTDIR" "/S=0K" "$0" "$1" "$2"
  IntFmt $0 "0x%08X" "$0"
  WriteRegDWORD HKLM "${UNINST_KEY}" "EstimatedSize" "$0"
SectionEnd

Section "开始菜单快捷方式" SecShortcuts
  CreateDirectory "$SMPROGRAMS\${PRODUCT_NAME}"
  CreateShortcut  "$SMPROGRAMS\${PRODUCT_NAME}\${PRODUCT_NAME}.lnk" \
                  "$INSTDIR\${PRODUCT_EXE}" "" "$INSTDIR\${PRODUCT_EXE}" 0
  CreateShortcut  "$SMPROGRAMS\${PRODUCT_NAME}\卸载.lnk" \
                  "$INSTDIR\Uninstall.exe"
SectionEnd

Section "桌面快捷方式" SecDesktop
  CreateShortcut "$DESKTOP\${PRODUCT_NAME}.lnk" \
                 "$INSTDIR\${PRODUCT_EXE}" "" "$INSTDIR\${PRODUCT_EXE}" 0
SectionEnd

; -------- 节描述 --------
LangString DESC_SecMain      ${LANG_SIMPCHINESE} "安装 BLAT 测试程序及全部配置文件。"
LangString DESC_SecShortcuts ${LANG_SIMPCHINESE} "在开始菜单创建快捷方式。"
LangString DESC_SecDesktop   ${LANG_SIMPCHINESE} "在桌面创建快捷方式。"
!insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
  !insertmacro MUI_DESCRIPTION_TEXT ${SecMain}      $(DESC_SecMain)
  !insertmacro MUI_DESCRIPTION_TEXT ${SecShortcuts} $(DESC_SecShortcuts)
  !insertmacro MUI_DESCRIPTION_TEXT ${SecDesktop}   $(DESC_SecDesktop)
!insertmacro MUI_FUNCTION_DESCRIPTION_END

Function .onInstSuccess
  MessageBox MB_ICONINFORMATION|MB_OK "$(^Name) 已成功安装到：$\r$\n$INSTDIR"
FunctionEnd

; -------- 卸载 --------
Section "Uninstall"
  ; Use built-in tasklist to detect a running instance — no FindProcDLL plugin
  ; dependency. tasklist exits 0 if a matching IMAGENAME was found, 1 otherwise.
  nsExec::ExecToLog 'tasklist /FI "IMAGENAME eq ${PRODUCT_EXE}" /NH'
  Pop $R0
  ${If} $R0 == 0
    MessageBox MB_ICONQUESTION|MB_YESNO|MB_DEFBUTTON2 \
      "${PRODUCT_NAME} 正在运行，是否结束并继续卸载？" IDYES +2
    Abort
    nsExec::ExecToLog '"taskkill" /F /IM "${PRODUCT_EXE}"'
    Sleep 1000
  ${EndIf}

  Delete "$INSTDIR\${PRODUCT_EXE}"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir /r "$INSTDIR\confs"
  RMDir "$INSTDIR"

  Delete "$SMPROGRAMS\${PRODUCT_NAME}\${PRODUCT_NAME}.lnk"
  Delete "$SMPROGRAMS\${PRODUCT_NAME}\卸载.lnk"
  RMDir  "$SMPROGRAMS\${PRODUCT_NAME}"
  Delete "$DESKTOP\${PRODUCT_NAME}.lnk"

  DeleteRegKey HKLM "Software\BLAT"
  DeleteRegKey HKLM "${UNINST_KEY}"
SectionEnd
