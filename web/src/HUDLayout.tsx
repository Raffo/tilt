import React from "react"
import styled from "styled-components"
import * as s from "./style-helpers"

// The HUD UI looks like this:
//
//            +----------------------------+---------+
//            | Header                     | Sidebar |
//            |                            |         |
//            +----------------------------+         |
//            |                            |         |
//            | Main                       |         |
//            |                            |         |
//            |                            |         |
//            |                            |         |
//            |                            |         |
//            +--------------------------------------+
//            +--------------------------------------+ Statusbar
//
// We need to satisfy several constraints:
//
// 1) Main streams logs and can grow very tall.
//    So we expect scrolling to be a common interaction.
//
// 2) Sidebar abuts Main, and is collapsible.
//    Sidebar never covers any content within HUDLayout.
//
// 3) Header and Statusbar may temporarily cover Main content,
//    but scrolling should make any covered content visible.
//
//
// To create this layout:
//
//    We're avoiding the approach of making Main `overflow: auto / scroll-y`.
//    Inner-div-scrolls can have lots of accessibility and UX issues.
//    (Nick can tell you hours of stories about this. e.g.,
//    https://medium.engineering/the-case-of-the-eternal-blur-ab350b9653ea)
//
//    HUDLayout has side padding that dynamically matches the Sidebar width,
//    and top + bottom padding to respectively match Header and Statusbar.
//    So when these latter elements are `position: fixed`, nothing is covered.
//
//    This way, scrolling anywhere on the page will scroll Main content.
//    (Unless you scroll atop the Sidebar, which _is_ `overflow: auto` 👀)

type HUDLayoutProps = {
  header: React.ReactNode
  children: React.ReactNode // Main pane
  isSidebarClosed: boolean
}

let Root = styled.div`
  display: flex;
  flex-direction: column;
  padding-top: ${s.Height.HUDheader}px;
  padding-right: ${s.Width.sidebar}px;
  padding-bottom: ${s.Height.statusbar}px;
  width: 100%;
  transition: padding-right ${s.AnimDuration.default} ease;
  box-sizing: border-box;

  &.is-sidebarCollapsed {
    padding-right: ${s.Width.sidebarCollapsed}px;
  }
`

let Header = styled.header`
  position: fixed;
  top: 0;
  left: 0;
  right: 0;
  padding-right: ${s.Width.sidebar}px;
  height: ${s.Height.HUDheader}px;
  background-color: ${s.Color.grayDarkest};
  box-shadow: inset 0px -2px 10px 0px rgba(${s.Color.black}, ${s.ColorAlpha.translucent});
  transition: padding-right ${s.AnimDuration.default} ease;
  z-index: ${s.ZIndex.HUDheader};

  .is-sidebarCollapsed & {
    padding-right: ${s.Width.sidebarCollapsed}px;
  }
`

let Main = styled.main``

export default function HUDLayout(props: HUDLayoutProps) {
  return (
    <Root className={props.isSidebarClosed ? "is-sidebarCollapsed" : ""}>
      <Header>{props.header}</Header>
      <Main>{props.children}</Main>
    </Root>
  )
}
