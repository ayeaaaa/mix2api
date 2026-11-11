# OpenWrt package for a prebuilt mix2api binary
#
# The release archives are portable tarballs. This SDK makefile is for
# operators who want a procd-managed package instead. Point MIX2API_BINARY at
# the `mix2api` file from the matching `openwrt-*` archive:
#
#   make package/mix2api/compile MIX2API_BINARY=/path/to/mix2api V=s
#
include $(TOPDIR)/rules.mk

PKG_NAME:=mix2api
PKG_VERSION:=$(if $(MIX2API_VERSION),$(MIX2API_VERSION),2.0.0)
PKG_RELEASE:=1
PKG_LICENSE:=MIT
PKG_MAINTAINER:=mix2api contributors
PKG_BUILD_DIR:=$(BUILD_DIR)/$(PKG_NAME)-$(PKG_VERSION)

MIX2API_BINARY?=

include $(INCLUDE_DIR)/package.mk

define Package/mix2api
  SECTION:=net
  CATEGORY:=Network
  SUBMENU:=Web Servers/Proxies
  TITLE:=mix2api multi-provider AI gateway
  URL:=https://github.com/ayeaaaa/mix2api
  DEPENDS:=+ca-bundle +ca-certificates +tzdata
  PKGARCH:=$(ARCH)
endef

define Package/mix2api/description
  Single-binary, CGO-free OpenAI/Anthropic-compatible gateway with an
  embedded dashboard. Data lives under /srv/mix2api by default.
endef

define Build/Prepare
	@test -n "$(MIX2API_BINARY)" || (echo "MIX2API_BINARY is required" && false)
	mkdir -p $(PKG_BUILD_DIR)
	$(CP) $(MIX2API_BINARY) $(PKG_BUILD_DIR)/mix2api
endef

define Build/Compile
endef

define Package/mix2api/conffiles
/etc/config/mix2api
endef

define Package/mix2api/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/mix2api $(1)/usr/bin/mix2api
	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/mix2api.init $(1)/etc/init.d/mix2api
	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/mix2api.config $(1)/etc/config/mix2api
	chmod 0600 $(1)/etc/config/mix2api
	$(INSTALL_DIR) $(1)/srv/mix2api/accounts
	$(INSTALL_DIR) $(1)/srv/mix2api/usage
	$(INSTALL_DIR) $(1)/srv/mix2api/media
endef

$(eval $(call BuildPackage,mix2api))
